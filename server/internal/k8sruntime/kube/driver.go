// Package kube implements controller.Driver against the Kubernetes REST API
// using only the standard library, so the controller carries no client-go
// dependency. It supports the handful of calls the controller needs.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/k8sruntime/controller"
)

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelDaemonID  = "multica.ai/daemon-id"
	managedByValue = "multica-k8s-controller"
	inputKey       = "task.json"
	inputDir       = "/run/multica"

	saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// Config describes the sandbox Pods the driver creates.
type Config struct {
	Namespace string
	DaemonID  string
	Image     string
	// PullPolicy is optional (Always, IfNotPresent, Never).
	PullPolicy string
	// RelayURL is what Pods use as MULTICA_SERVER_URL. It must point at the
	// relay, never at the real server.
	RelayURL string
	// RuntimeClassName selects the isolation runtime (gVisor, Kata...). Empty
	// uses the cluster default.
	RuntimeClassName string
	NodeSelector     map[string]string
	// Resource quantities in Kubernetes notation; empty leaves them unset.
	CPURequest, CPULimit, MemoryRequest, MemoryLimit string
	// WorkspaceSizeLimit caps the per-task scratch volume (default 10Gi).
	WorkspaceSizeLimit string
	// EnvFromSecrets are existing Secrets exposed as environment variables, for
	// provider API keys and git credentials (GH_TOKEN, ...).
	EnvFromSecrets []string
	ExtraEnv       map[string]string
	// ActiveDeadlineSeconds is a hard wall-clock backstop enforced by the
	// kubelet; 0 disables it.
	ActiveDeadlineSeconds int64
}

// Driver talks to the Kubernetes API.
type Driver struct {
	cfg   Config
	base  string
	http  *http.Client
	token func() (string, error)
}

// New returns a Driver for an explicit API endpoint. token may be nil.
func New(cfg Config, base string, client *http.Client, token func() (string, error)) (*Driver, error) {
	if cfg.Namespace == "" || cfg.Image == "" || cfg.RelayURL == "" || cfg.DaemonID == "" {
		return nil, errors.New("kube: namespace, image, relay URL and daemon id are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Driver{cfg: cfg, base: strings.TrimRight(base, "/"), http: client, token: token}, nil
}

// NewInCluster builds a Driver from the Pod's service account.
func NewInCluster(cfg Config) (*Driver, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("kube: not running in a cluster (KUBERNETES_SERVICE_HOST unset)")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("kube: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("kube: no certificates in service account CA")
	}
	if cfg.Namespace == "" {
		ns, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			return nil, fmt.Errorf("kube: read namespace: %w", err)
		}
		cfg.Namespace = strings.TrimSpace(string(ns))
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	// The projected token rotates, so read it on every request.
	token := func() (string, error) {
		b, err := os.ReadFile(saDir + "/token")
		return strings.TrimSpace(string(b)), err
	}
	return New(cfg, "https://"+host+":"+port, client, token)
}

var errNotFound = errors.New("not found")

// apiError is a non-success Kubernetes API response.
type apiError struct {
	status int
	msg    string
	body   string
}

func (e *apiError) Error() string { return e.msg }

// isCapacity reports whether err means "cannot create a Pod right now, but it
// may work later": an exhausted namespace quota, throttling, or a briefly
// unavailable API. Other 403s (for example missing RBAC) are permanent.
func isCapacity(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusForbidden:
		return strings.Contains(ae.body, "exceeded quota")
	}
	return false
}

func (d *Driver) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	u := d.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if d.token != nil {
		tok, err := d.token()
		if err != nil {
			return fmt.Errorf("kube: read token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode == http.StatusConflict:
		return errConflict
	case resp.StatusCode >= 300:
		body := strings.TrimSpace(string(data))
		return &apiError{status: resp.StatusCode, body: body, msg: fmt.Sprintf("kube: %s %s: %s: %s", method, path, resp.Status, body)}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

var errConflict = errors.New("already exists")

func (d *Driver) labels() map[string]string {
	return map[string]string{labelManagedBy: managedByValue, labelDaemonID: d.cfg.DaemonID}
}

func (d *Driver) podsPath() string    { return "/api/v1/namespaces/" + d.cfg.Namespace + "/pods" }
func (d *Driver) secretsPath() string { return "/api/v1/namespaces/" + d.cfg.Namespace + "/secrets" }

// Create stores the runner input in a Secret and creates the Pod. Both calls
// treat AlreadyExists as success, so a retry after a crash is safe.
func (d *Driver) Create(ctx context.Context, spec controller.PodSpec) error {
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": spec.Name, "labels": d.labels()},
		"data":     map[string]string{inputKey: base64.StdEncoding.EncodeToString(spec.Input)},
	}
	if err := d.do(ctx, http.MethodPost, d.secretsPath(), nil, secret, nil); err != nil && !errors.Is(err, errConflict) {
		return fmt.Errorf("create input secret: %w", err)
	}
	if err := d.do(ctx, http.MethodPost, d.podsPath(), nil, d.podManifest(spec), nil); err != nil && !errors.Is(err, errConflict) {
		if isCapacity(err) {
			// Keep the input Secret: the controller retries the same Create.
			return fmt.Errorf("create pod: %w: %v", controller.ErrNoCapacity, err)
		}
		_ = d.do(ctx, http.MethodDelete, d.secretsPath()+"/"+spec.Name, nil, nil, nil)
		return fmt.Errorf("create pod: %w", err)
	}
	return nil
}

func (d *Driver) podManifest(spec controller.PodSpec) map[string]any {
	c := d.cfg
	sizeLimit := c.WorkspaceSizeLimit
	if sizeLimit == "" {
		sizeLimit = "10Gi"
	}

	env := []map[string]any{
		{"name": "MULTICA_SERVER_URL", "value": c.RelayURL},
		{"name": "MULTICA_DAEMON_ID", "value": c.DaemonID},
		{"name": "MULTICA_WORKSPACES_ROOT", "value": "/workspaces"},
		{"name": "MULTICA_DAEMON_MAX_CONCURRENT_TASKS", "value": "1"},
		{"name": "MULTICA_DAEMON_AUTO_UPDATE", "value": "false"},
		{"name": "MULTICA_TASK_INPUT", "value": inputDir + "/" + inputKey},
		{"name": "HOME", "value": "/home/runner"},
	}
	for k, v := range c.ExtraEnv {
		env = append(env, map[string]any{"name": k, "value": v})
	}
	envFrom := make([]map[string]any, 0, len(c.EnvFromSecrets))
	for _, s := range append(append([]string{}, c.EnvFromSecrets...), spec.EnvFromSecrets...) {
		envFrom = append(envFrom, map[string]any{"secretRef": map[string]any{"name": s}})
	}

	resources := map[string]any{}
	req, lim := map[string]string{}, map[string]string{}
	set := func(m map[string]string, k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	set(req, "cpu", c.CPURequest)
	set(req, "memory", c.MemoryRequest)
	set(lim, "cpu", c.CPULimit)
	set(lim, "memory", c.MemoryLimit)
	if len(req) > 0 {
		resources["requests"] = req
	}
	if len(lim) > 0 {
		resources["limits"] = lim
	}

	image := c.Image
	if spec.Image != "" {
		image = spec.Image
	}
	container := map[string]any{
		"name":    "runner",
		"image":   image,
		"command": []string{"/app/multica-k8s-runner"},
		"env":     env,
		"envFrom": envFrom,
		"volumeMounts": []map[string]any{
			{"name": "workspaces", "mountPath": "/workspaces"},
			{"name": "home", "mountPath": "/home/runner"},
			{"name": "tmp", "mountPath": "/tmp"},
			{"name": "input", "mountPath": inputDir, "readOnly": true},
		},
		"resources": resources,
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"capabilities":             map[string]any{"drop": []string{"ALL"}},
			"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
		},
	}
	if c.PullPolicy != "" {
		container["imagePullPolicy"] = c.PullPolicy
	}

	podSpec := map[string]any{
		"restartPolicy": "Never",
		// The agent runs arbitrary code; it gets no Kubernetes API access.
		"automountServiceAccountToken": false,
		"enableServiceLinks":           false,
		"securityContext":              map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000},
		"containers":                   []map[string]any{container},
		"volumes": []map[string]any{
			{"name": "workspaces", "emptyDir": map[string]any{"sizeLimit": sizeLimit}},
			{"name": "home", "emptyDir": map[string]any{}},
			{"name": "tmp", "emptyDir": map[string]any{}},
			{"name": "input", "secret": map[string]any{"secretName": spec.Name, "defaultMode": 0o400}},
		},
	}
	if c.RuntimeClassName != "" {
		podSpec["runtimeClassName"] = c.RuntimeClassName
	}
	if len(c.NodeSelector) > 0 {
		podSpec["nodeSelector"] = c.NodeSelector
	}
	if c.ActiveDeadlineSeconds > 0 {
		podSpec["activeDeadlineSeconds"] = c.ActiveDeadlineSeconds
	}

	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": spec.Name, "labels": d.labels(), "annotations": spec.Annotations},
		"spec":     podSpec,
	}
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name              string            `json:"name"`
			Annotations       map[string]string `json:"annotations"`
			CreationTimestamp time.Time         `json:"creationTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase             string `json:"phase"`
			Reason            string `json:"reason"`
			ContainerStatuses []struct {
				State struct {
					Terminated *struct {
						Reason   string `json:"reason"`
						ExitCode int    `json:"exitCode"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// List returns this controller's sandbox Pods.
func (d *Driver) List(ctx context.Context) ([]controller.PodInfo, error) {
	q := url.Values{"labelSelector": {labelManagedBy + "=" + managedByValue + "," + labelDaemonID + "=" + d.cfg.DaemonID}}
	var pl podList
	if err := d.do(ctx, http.MethodGet, d.podsPath(), q, nil, &pl); err != nil {
		return nil, err
	}
	out := make([]controller.PodInfo, 0, len(pl.Items))
	for _, it := range pl.Items {
		info := controller.PodInfo{
			Name:        it.Metadata.Name,
			Annotations: it.Metadata.Annotations,
			CreatedAt:   it.Metadata.CreationTimestamp,
			Phase:       phase(it.Status.Phase),
			Reason:      it.Status.Reason,
		}
		for _, cs := range it.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				info.Reason = fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func phase(p string) controller.Phase {
	switch p {
	case "Pending":
		return controller.PhasePending
	case "Succeeded":
		return controller.PhaseSucceeded
	case "Failed":
		return controller.PhaseFailed
	default: // Running, Unknown
		return controller.PhaseRunning
	}
}

// Input reads the runner input back from the Pod's Secret.
func (d *Driver) Input(ctx context.Context, name string) ([]byte, error) {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := d.do(ctx, http.MethodGet, d.secretsPath()+"/"+name, nil, nil, &s); err != nil {
		return nil, err
	}
	raw, ok := s.Data[inputKey]
	if !ok {
		return nil, errors.New("kube: input secret has no task.json")
	}
	return base64.StdEncoding.DecodeString(raw)
}

// Delete removes the Pod and its Secret; missing objects are success.
func (d *Driver) Delete(ctx context.Context, name string) error {
	grace := url.Values{"gracePeriodSeconds": {"10"}}
	if err := d.do(ctx, http.MethodDelete, d.podsPath()+"/"+name, grace, nil, nil); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	if err := d.do(ctx, http.MethodDelete, d.secretsPath()+"/"+name, nil, nil, nil); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	return nil
}

var _ controller.Driver = (*Driver)(nil)
