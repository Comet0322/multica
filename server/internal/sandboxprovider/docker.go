package sandboxprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	dockerAPIVersion   = "v1.41"
	labelManaged       = "ai.multica.sandbox"
	labelInstance      = "ai.multica.sandbox.instance"
	labelKey           = "ai.multica.sandbox.key"
	defaultVolumeMount = "/data"
	dockerStopSeconds  = 20
)

// DockerConfig talks to the Docker Engine API directly so the broker needs
// only the socket, not the docker CLI.
type DockerConfig struct {
	// Instance namespaces resource names and labels so several brokers can
	// share one Docker host. It must itself be a valid key.
	Instance string
	// SocketPath defaults to /var/run/docker.sock. Ignored when BaseURL is set.
	SocketPath string
	// BaseURL and HTTPClient override the transport; tests use them.
	BaseURL    string
	HTTPClient *http.Client

	VolumeMount string
	Network     string
	ExtraHosts  []string
	// Runtime selects an OCI runtime such as "runsc" (gVisor).
	Runtime  string
	MemoryMB int64
	CPUs     float64
}

type Docker struct {
	cfg    DockerConfig
	base   string
	client *http.Client
}

func NewDocker(cfg DockerConfig) (*Docker, error) {
	if !ValidKey(cfg.Instance) {
		return nil, fmt.Errorf("docker instance: %w", ErrInvalidKey)
	}
	if cfg.VolumeMount == "" {
		cfg.VolumeMount = defaultVolumeMount
	}
	d := &Docker{cfg: cfg, base: strings.TrimRight(cfg.BaseURL, "/"), client: cfg.HTTPClient}
	if d.base == "" {
		socket := cfg.SocketPath
		if socket == "" {
			socket = "/var/run/docker.sock"
		}
		d.base = "http://docker"
		d.client = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		}}
	}
	if d.client == nil {
		d.client = http.DefaultClient
	}
	return d, nil
}

func (d *Docker) name(key string) string { return "multica-sbx-" + d.cfg.Instance + "-" + key }

type dockerContainer struct {
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status    string `json:"Status"`
		ExitCode  int    `json:"ExitCode"`
		Error     string `json:"Error"`
		OOMKilled bool   `json:"OOMKilled"`
	} `json:"State"`
}

func (d *Docker) Ensure(ctx context.Context, spec Spec) (Sandbox, error) {
	if !ValidKey(spec.Key) {
		return Sandbox{}, ErrInvalidKey
	}
	if spec.Image == "" {
		return Sandbox{}, errors.New("sandbox image is required")
	}
	current, err := d.inspect(ctx, spec.Key)
	if err != nil {
		return Sandbox{}, err
	}
	if current != nil && current.Config.Image != spec.Image {
		if err := d.removeContainer(ctx, spec.Key); err != nil {
			return Sandbox{}, err
		}
		current = nil
	}
	if current == nil {
		if err := d.create(ctx, spec); err != nil {
			return Sandbox{}, err
		}
	}
	if current == nil || current.State.Status != "running" {
		if err := d.do(ctx, http.MethodPost, "/containers/"+d.name(spec.Key)+"/start", nil, nil, http.StatusNoContent, http.StatusNotModified); err != nil {
			return Sandbox{}, fmt.Errorf("start sandbox: %w", err)
		}
	}
	return d.Get(ctx, spec.Key)
}

func (d *Docker) create(ctx context.Context, spec Spec) error {
	name := d.name(spec.Key)
	labels := map[string]string{}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	labels[labelManaged] = "1"
	labels[labelInstance] = d.cfg.Instance
	labels[labelKey] = spec.Key
	if err := d.do(ctx, http.MethodPost, "/volumes/create", map[string]any{"Name": name, "Labels": labels}, nil, http.StatusCreated, http.StatusOK); err != nil {
		return fmt.Errorf("create sandbox volume: %w", err)
	}
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	host := map[string]any{
		"Mounts":        []map[string]any{{"Type": "volume", "Source": name, "Target": d.cfg.VolumeMount}},
		"RestartPolicy": map[string]any{"Name": "no"},
	}
	if d.cfg.Network != "" {
		host["NetworkMode"] = d.cfg.Network
	}
	if len(d.cfg.ExtraHosts) > 0 {
		host["ExtraHosts"] = d.cfg.ExtraHosts
	}
	if d.cfg.Runtime != "" {
		host["Runtime"] = d.cfg.Runtime
	}
	if d.cfg.MemoryMB > 0 {
		host["Memory"] = d.cfg.MemoryMB << 20
	}
	if d.cfg.CPUs > 0 {
		host["NanoCpus"] = int64(d.cfg.CPUs * 1e9)
	}
	body := map[string]any{"Image": spec.Image, "Env": env, "Labels": labels, "HostConfig": host}
	path := "/containers/create?name=" + url.QueryEscape(name)
	err := d.do(ctx, http.MethodPost, path, body, nil, http.StatusCreated)
	var apiErr *dockerError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		if err := d.pull(ctx, spec.Image); err != nil {
			return err
		}
		err = d.do(ctx, http.MethodPost, path, body, nil, http.StatusCreated)
	}
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		// A concurrent Ensure created it first.
		return nil
	}
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	return nil
}

// pull drains the progress stream; Docker reports pull failures inside it.
func (d *Docker) pull(ctx context.Context, image string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/"+dockerAPIVersion+"/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("pull sandbox image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pull sandbox image: %w", readDockerError(resp))
	}
	decoder := json.NewDecoder(resp.Body)
	for {
		var event struct {
			Error string `json:"error"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("pull sandbox image: %w", err)
		}
		if event.Error != "" {
			return fmt.Errorf("pull sandbox image: %s", event.Error)
		}
	}
}

func (d *Docker) Get(ctx context.Context, key string) (Sandbox, error) {
	if !ValidKey(key) {
		return Sandbox{}, ErrInvalidKey
	}
	c, err := d.inspect(ctx, key)
	if err != nil {
		return Sandbox{}, err
	}
	if c == nil {
		return Sandbox{Key: key, Status: StatusMissing}, nil
	}
	return dockerSandbox(key, c.Config.Image, c.State.Status, c.State.ExitCode, c.State.Error, c.State.OOMKilled), nil
}

func dockerSandbox(key, image, state string, exitCode int, stateErr string, oom bool) Sandbox {
	s := Sandbox{Key: key, Image: image}
	switch state {
	case "running":
		s.Status = StatusRunning
	case "created", "restarting":
		s.Status = StatusPending
	case "exited", "paused":
		// A stop requested by the broker also exits non-zero (SIGTERM), so
		// only the broker can tell an expected stop from a crash.
		s.Status = StatusStopped
		s.Reason = fmt.Sprintf("exit code %d", exitCode)
	default:
		s.Status = StatusFailed
		s.Reason = "container state " + state
	}
	if oom {
		s.Status, s.Reason = StatusFailed, "out of memory"
	} else if stateErr != "" {
		s.Status, s.Reason = StatusFailed, stateErr
	}
	return s
}

func (d *Docker) Stop(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	err := d.do(ctx, http.MethodPost, fmt.Sprintf("/containers/%s/stop?t=%d", d.name(key), dockerStopSeconds), nil, nil, http.StatusNoContent, http.StatusNotModified, http.StatusNotFound)
	if err != nil {
		return fmt.Errorf("stop sandbox: %w", err)
	}
	return nil
}

func (d *Docker) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	if err := d.removeContainer(ctx, key); err != nil {
		return err
	}
	if err := d.do(ctx, http.MethodDelete, "/volumes/"+d.name(key)+"?force=1", nil, nil, http.StatusNoContent, http.StatusNotFound); err != nil {
		return fmt.Errorf("delete sandbox volume: %w", err)
	}
	return nil
}

func (d *Docker) removeContainer(ctx context.Context, key string) error {
	if err := d.do(ctx, http.MethodDelete, "/containers/"+d.name(key)+"?force=1", nil, nil, http.StatusNoContent, http.StatusNotFound); err != nil {
		return fmt.Errorf("remove sandbox container: %w", err)
	}
	return nil
}

func (d *Docker) List(ctx context.Context) ([]Sandbox, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {labelInstance + "=" + d.cfg.Instance}})
	var containers []struct {
		Image  string            `json:"Image"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil, &containers, http.StatusOK); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	out := make([]Sandbox, 0, len(containers))
	for _, c := range containers {
		key := c.Labels[labelKey]
		if key == "" {
			continue
		}
		// The list endpoint omits exit details; Get has them.
		out = append(out, dockerSandbox(key, c.Image, c.State, 0, "", false))
	}
	return out, nil
}

func (d *Docker) inspect(ctx context.Context, key string) (*dockerContainer, error) {
	var c dockerContainer
	err := d.do(ctx, http.MethodGet, "/containers/"+d.name(key)+"/json", nil, &c, http.StatusOK)
	var apiErr *dockerError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect sandbox: %w", err)
	}
	return &c, nil
}

type dockerError struct {
	Status  int
	Message string
}

func (e *dockerError) Error() string {
	return fmt.Sprintf("docker API returned HTTP %d: %s", e.Status, e.Message)
}

func readDockerError(resp *http.Response) error {
	var body struct {
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(raw, &body)
	return &dockerError{Status: resp.StatusCode, Message: body.Message}
}

func (d *Docker) do(ctx context.Context, method, path string, in, out any, okStatus ...int) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, d.base+"/"+dockerAPIVersion+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, status := range okStatus {
		if resp.StatusCode != status {
			continue
		}
		if out != nil && resp.StatusCode == http.StatusOK {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return readDockerError(resp)
}
