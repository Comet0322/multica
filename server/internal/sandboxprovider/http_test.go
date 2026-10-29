package sandboxprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// memProvider is an in-memory Provider for exercising the HTTP adapter.
type memProvider struct {
	mu    sync.Mutex
	boxes map[string]Sandbox
}

func (m *memProvider) Ensure(_ context.Context, spec Spec) (Sandbox, error) {
	if !ValidKey(spec.Key) {
		return Sandbox{}, ErrInvalidKey
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Sandbox{Key: spec.Key, Status: StatusRunning, Image: spec.Image}
	m.boxes[spec.Key] = s
	return s, nil
}

func (m *memProvider) Get(_ context.Context, key string) (Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.boxes[key]; ok {
		return s, nil
	}
	return Sandbox{Key: key, Status: StatusMissing}, nil
}

func (m *memProvider) Stop(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.boxes[key]; ok {
		s.Status = StatusStopped
		m.boxes[key] = s
	}
	return nil
}

func (m *memProvider) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.boxes, key)
	return nil
}

func (m *memProvider) List(context.Context) ([]Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Sandbox
	for _, s := range m.boxes {
		out = append(out, s)
	}
	return out, nil
}

func TestHTTPProviderRoundTripsThroughReferenceHandler(t *testing.T) {
	srv := httptest.NewServer(NewHTTPHandler(&memProvider{boxes: map[string]Sandbox{}}, "secret"))
	defer srv.Close()
	p, err := NewHTTP(HTTPConfig{BaseURL: srv.URL + "/", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if s, err := p.Get(ctx, "issue-1"); err != nil || s.Status != StatusMissing {
		t.Fatalf("unknown sandbox = %+v, %v", s, err)
	}
	s, err := p.Ensure(ctx, Spec{Key: "issue-1", Image: "img:1", Env: map[string]string{"A": "1"}})
	if err != nil || s.Status != StatusRunning || s.Image != "img:1" || s.Key != "issue-1" {
		t.Fatalf("ensure = %+v, %v", s, err)
	}
	if err := p.Stop(ctx, "issue-1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := p.Get(ctx, "issue-1"); s.Status != StatusStopped {
		t.Fatalf("after stop = %+v", s)
	}
	if list, err := p.List(ctx); err != nil || len(list) != 1 || list[0].Key != "issue-1" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := p.Delete(ctx, "issue-1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := p.Get(ctx, "issue-1"); s.Status != StatusMissing {
		t.Fatalf("after delete = %+v", s)
	}

	bad, _ := NewHTTP(HTTPConfig{BaseURL: srv.URL, Token: "wrong"})
	var serviceErr *ServiceError
	if _, err := bad.List(ctx); !errors.As(err, &serviceErr) || serviceErr.Status != http.StatusUnauthorized {
		t.Fatalf("wrong token err = %v", err)
	}
	if _, err := p.Ensure(ctx, Spec{Key: "Bad_Key", Image: "img"}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid key err = %v", err)
	}
}

func TestHTTPProviderToleratesServiceVariations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sandboxes/gone":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/sandboxes/odd":
			w.Write([]byte(`{"key":"odd","status":"hibernating"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"cluster full"}`))
		}
	}))
	defer srv.Close()
	p, _ := NewHTTP(HTTPConfig{BaseURL: srv.URL, Token: "t"})
	ctx := context.Background()
	if s, err := p.Get(ctx, "gone"); err != nil || s.Status != StatusMissing {
		t.Fatalf("404 should mean missing: %+v %v", s, err)
	}
	if s, _ := p.Get(ctx, "odd"); s.Status != StatusPending {
		t.Fatalf("unknown status should be treated as pending: %+v", s)
	}
	var serviceErr *ServiceError
	if _, err := p.Ensure(ctx, Spec{Key: "full", Image: "img"}); !errors.As(err, &serviceErr) || serviceErr.Message != "cluster full" {
		t.Fatalf("service error = %v", err)
	}
}
