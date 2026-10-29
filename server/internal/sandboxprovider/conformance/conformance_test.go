package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxprovider"
)

// simProvider simulates a sandbox service: "running" a sandbox executes the
// probe's logic in-process against an in-memory volume.
type simProvider struct {
	mu      sync.Mutex
	boxes   map[string]*simBox
	volumes map[string]string
	// stopDeletesVolume breaks the contract on purpose.
	stopDeletesVolume bool
}

type simBox struct {
	status sandboxprovider.Status
	spec   sandboxprovider.Spec
}

func sameEnv(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (p *simProvider) Ensure(_ context.Context, spec sandboxprovider.Spec) (sandboxprovider.Sandbox, error) {
	if !sandboxprovider.ValidKey(spec.Key) {
		return sandboxprovider.Sandbox{}, sandboxprovider.ErrInvalidKey
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	box := p.boxes[spec.Key]
	if box != nil && box.status == sandboxprovider.StatusRunning && box.spec.Image == spec.Image && sameEnv(box.spec.Env, spec.Env) {
		return sandboxprovider.Sandbox{Key: spec.Key, Status: box.status, Image: spec.Image}, nil
	}
	p.boxes[spec.Key] = &simBox{status: sandboxprovider.StatusRunning, spec: spec}
	previous, ok := p.volumes[spec.Key]
	if !ok {
		previous = "none"
	}
	p.volumes[spec.Key] = spec.Env["CONFORMANCE_NONCE"]
	rep := Report{
		Key: spec.Env["CONFORMANCE_KEY"], Nonce: spec.Env["CONFORMANCE_NONCE"], Echo: spec.Env[spec.Env["CONFORMANCE_ECHO_VAR"]],
		Previous: previous, UID: "1000", Volume: "ok", Tmp: "ok", Rootfs: "read-only", Egress: "blocked",
		CapBnd: "0000000000000000", NoNewPrivs: "1",
	}
	go func() {
		body, _ := json.Marshal(rep)
		resp, err := http.Post(spec.Env["CONFORMANCE_CALLBACK"]+"/report", "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
	}()
	return sandboxprovider.Sandbox{Key: spec.Key, Status: sandboxprovider.StatusRunning, Image: spec.Image}, nil
}

func (p *simProvider) Get(_ context.Context, key string) (sandboxprovider.Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if box := p.boxes[key]; box != nil {
		return sandboxprovider.Sandbox{Key: key, Status: box.status, Image: box.spec.Image}, nil
	}
	return sandboxprovider.Sandbox{Key: key, Status: sandboxprovider.StatusMissing}, nil
}

func (p *simProvider) Stop(_ context.Context, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if box := p.boxes[key]; box != nil {
		box.status = sandboxprovider.StatusStopped
	}
	if p.stopDeletesVolume {
		delete(p.volumes, key)
	}
	return nil
}

func (p *simProvider) Delete(_ context.Context, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.boxes, key)
	delete(p.volumes, key)
	return nil
}

func (p *simProvider) List(context.Context) ([]sandboxprovider.Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []sandboxprovider.Sandbox
	for key, box := range p.boxes {
		out = append(out, sandboxprovider.Sandbox{Key: key, Status: box.status, Image: box.spec.Image})
	}
	return out, nil
}

func runAgainst(t *testing.T, sim *simProvider) []Result {
	t.Helper()
	collector := NewCollector()
	callback := httptest.NewServer(collector)
	defer callback.Close()
	service := httptest.NewServer(sandboxprovider.NewHTTPHandler(sim, "token"))
	defer service.Close()
	provider, err := sandboxprovider.NewHTTP(sandboxprovider.HTTPConfig{BaseURL: service.URL, Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	results := CheckHTTPAPI(ctx, service.URL, "token")
	return append(results, Run(ctx, provider, collector, Config{
		Image: "probe", CallbackURL: callback.URL, EgressURL: "https://example.com",
		StartTimeout: 5 * time.Second, ReportTimeout: 5 * time.Second, IdleWindow: 100 * time.Millisecond,
	})...)
}

func TestReferenceHandlerPassesConformance(t *testing.T) {
	sim := &simProvider{boxes: map[string]*simBox{}, volumes: map[string]string{}}
	results := runAgainst(t, sim)
	for _, r := range results {
		if r.Level != Pass {
			t.Errorf("%s %s: %s", r.Level, r.Name, r.Message)
		}
	}
	if len(sim.boxes) != 0 || len(sim.volumes) != 0 {
		t.Fatalf("the suite must clean up: boxes=%v volumes=%v", sim.boxes, sim.volumes)
	}
}

func TestConformanceCatchesLostVolume(t *testing.T) {
	results := runAgainst(t, &simProvider{boxes: map[string]*simBox{}, volumes: map[string]string{}, stopDeletesVolume: true})
	if !Failed(results) {
		t.Fatal("a service that drops volumes on stop must fail")
	}
	last := results[len(results)-1]
	if last.Name != "stop then PUT keeps the volume" || last.Level != Fail {
		t.Fatalf("expected the stop check to fail and end the run, got %+v", last)
	}
}
