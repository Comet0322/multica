package sandboxprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeEngine models just enough Docker Engine API state for the provider.
type fakeEngine struct {
	mu         sync.Mutex
	containers map[string]map[string]any
	volumes    map[string]bool
	images     map[string]bool
	requests   []string
}

func (e *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/"+dockerAPIVersion)
	e.requests = append(e.requests, r.Method+" "+path)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	reply := func(status int, v any) {
		w.WriteHeader(status)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	switch {
	case path == "/volumes/create":
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		e.volumes[body.Name] = true
		reply(http.StatusCreated, nil)
	case path == "/images/create":
		e.images[r.URL.Query().Get("fromImage")] = true
		io.WriteString(w, `{"status":"pulled"}`+"\n")
	case path == "/containers/create":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !e.images[body["Image"].(string)] {
			reply(http.StatusNotFound, map[string]string{"message": "No such image"})
			return
		}
		e.containers[r.URL.Query().Get("name")] = map[string]any{"spec": body, "state": "created"}
		reply(http.StatusCreated, map[string]string{"Id": "x"})
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "start":
		e.containers[parts[1]]["state"] = "running"
		reply(http.StatusNoContent, nil)
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "stop":
		if c := e.containers[parts[1]]; c != nil {
			c["state"] = "exited"
		}
		reply(http.StatusNoContent, nil)
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "json":
		c := e.containers[parts[1]]
		if c == nil {
			reply(http.StatusNotFound, map[string]string{"message": "No such container"})
			return
		}
		spec := c["spec"].(map[string]any)
		reply(http.StatusOK, map[string]any{
			"Config": map[string]any{"Image": spec["Image"], "Labels": spec["Labels"]},
			"State":  map[string]any{"Status": c["state"], "ExitCode": 143},
		})
	case r.Method == http.MethodDelete && parts[0] == "containers":
		delete(e.containers, parts[1])
		reply(http.StatusNoContent, nil)
	case r.Method == http.MethodDelete && parts[0] == "volumes":
		delete(e.volumes, parts[1])
		reply(http.StatusNoContent, nil)
	default:
		reply(http.StatusNotImplemented, map[string]string{"message": path})
	}
}

func TestDockerLimitsCanBeRemovedAndRootfsMadeWritable(t *testing.T) {
	engine := &fakeEngine{containers: map[string]map[string]any{}, volumes: map[string]bool{}, images: map[string]bool{"img": true}}
	srv := httptest.NewServer(engine)
	defer srv.Close()
	d, err := NewDocker(DockerConfig{Instance: "test", BaseURL: srv.URL, MemoryMB: -1, CPUs: -1, PidsLimit: -1, WritableRootfs: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Ensure(context.Background(), Spec{Key: "k", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	host := engine.containers["multica-sbx-test-k"]["spec"].(map[string]any)["HostConfig"].(map[string]any)
	for _, field := range []string{"Memory", "NanoCpus", "PidsLimit"} {
		if _, ok := host[field]; ok {
			t.Fatalf("%s should be unset, got %v", field, host[field])
		}
	}
	if host["ReadonlyRootfs"] != false || host["CapDrop"] == nil {
		t.Fatalf("only the rootfs should become writable: %v", host)
	}
}

func TestDockerLifecycleKeepsVolumeAcrossStopAndImageChange(t *testing.T) {
	engine := &fakeEngine{containers: map[string]map[string]any{}, volumes: map[string]bool{}, images: map[string]bool{"img:2": true}}
	srv := httptest.NewServer(engine)
	defer srv.Close()
	d, err := NewDocker(DockerConfig{Instance: "test", BaseURL: srv.URL, HTTPClient: srv.Client(), Runtime: "runsc", MemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	name := "multica-sbx-test-agent-1"

	if s, _ := d.Get(ctx, "agent-1"); s.Status != StatusMissing {
		t.Fatalf("status before create = %s", s.Status)
	}
	s, err := d.Ensure(ctx, Spec{Key: "agent-1", Image: "img:1", Env: map[string]string{"A": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusRunning || !engine.volumes[name] || !engine.images["img:1"] {
		t.Fatalf("ensure = %+v, volumes=%v images=%v", s, engine.volumes, engine.images)
	}
	spec := engine.containers[name]["spec"].(map[string]any)
	host := spec["HostConfig"].(map[string]any)
	if host["Runtime"] != "runsc" || host["Memory"].(float64) != 512<<20 || spec["Env"].([]any)[0] != "A=1" {
		t.Fatalf("container spec = %v", spec)
	}
	if host["NanoCpus"].(float64) != 2e9 || host["PidsLimit"].(float64) != 1024 {
		t.Fatalf("unset limits should get defaults: %v", host)
	}
	if caps := host["CapDrop"].([]any); len(caps) != 1 || caps[0] != "ALL" {
		t.Fatalf("CapDrop = %v", host["CapDrop"])
	}
	if opts := host["SecurityOpt"].([]any); len(opts) != 1 || opts[0] != "no-new-privileges:true" {
		t.Fatalf("SecurityOpt = %v", host["SecurityOpt"])
	}
	if host["ReadonlyRootfs"] != true || host["Tmpfs"].(map[string]any)["/tmp"] == nil {
		t.Fatalf("root filesystem should be read-only with a /tmp tmpfs: %v", host)
	}

	if err := d.Stop(ctx, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.Get(ctx, "agent-1"); s.Status != StatusStopped {
		t.Fatalf("status after stop = %s", s.Status)
	}
	if _, err := d.Ensure(ctx, Spec{Key: "agent-1", Image: "img:2"}); err != nil {
		t.Fatal(err)
	}
	if got := engine.containers[name]["spec"].(map[string]any)["Image"]; got != "img:2" || !engine.volumes[name] {
		t.Fatalf("image change should recreate the container and keep the volume: image=%v volume=%v", got, engine.volumes[name])
	}

	// A config change (here: memory) replaces the compute and keeps the volume.
	d2, _ := NewDocker(DockerConfig{Instance: "test", BaseURL: srv.URL, HTTPClient: srv.Client(), Runtime: "runsc", MemoryMB: 1024})
	before := engine.containers[name]["spec"].(map[string]any)["Labels"].(map[string]any)[labelSpec]
	if _, err := d2.Ensure(ctx, Spec{Key: "agent-1", Image: "img:2"}); err != nil {
		t.Fatal(err)
	}
	after := engine.containers[name]["spec"].(map[string]any)
	if after["HostConfig"].(map[string]any)["Memory"].(float64) != 1024<<20 || after["Labels"].(map[string]any)[labelSpec] == before || !engine.volumes[name] {
		t.Fatalf("spec change should recreate the container with the volume kept: %v", after)
	}

	if err := d.Delete(ctx, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if len(engine.containers) != 0 || len(engine.volumes) != 0 {
		t.Fatal("delete should remove container and volume")
	}
	if _, err := d.Ensure(ctx, Spec{Key: "Bad_Key", Image: "img:2"}); err != ErrInvalidKey {
		t.Fatalf("invalid key err = %v", err)
	}
}
