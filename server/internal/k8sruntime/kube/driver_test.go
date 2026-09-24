package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/k8sruntime/controller"
)

// fakeAPI is a tiny in-memory Kubernetes API for pods and secrets.
type fakeAPI struct {
	mu      sync.Mutex
	pods    map[string]map[string]any
	secrets map[string]map[string]any
	calls   []string
	auth    string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{pods: map[string]map[string]any{}, secrets: map[string]map[string]any{}}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	store := f.pods
	kind := "pods"
	if strings.Contains(r.URL.Path, "/secrets") {
		store, kind = f.secrets, "secrets"
	}
	base := "/api/v1/namespaces/ns/" + kind
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")

	switch r.Method {
	case http.MethodPost:
		var obj map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &obj)
		n := obj["metadata"].(map[string]any)["name"].(string)
		if _, dup := store[n]; dup {
			http.Error(w, "exists", http.StatusConflict)
			return
		}
		store[n] = obj
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	case http.MethodGet:
		if name == "" { // list pods
			items := []map[string]any{}
			for n, p := range store {
				items = append(items, map[string]any{
					"metadata": map[string]any{"name": n, "annotations": p["metadata"].(map[string]any)["annotations"], "creationTimestamp": "2026-01-02T03:04:05Z"},
					"status": map[string]any{"phase": p["__phase"], "containerStatuses": []map[string]any{
						{"state": map[string]any{"terminated": map[string]any{"reason": "OOMKilled", "exitCode": 137}}},
					}},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
			return
		}
		obj, ok := store[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(obj)
	case http.MethodDelete:
		if _, ok := store[name]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(store, name)
	}
}

func newDriver(t *testing.T, api *fakeAPI, mutate func(*Config)) *Driver {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cfg := Config{Namespace: "ns", DaemonID: "d1", Image: "img:1", RelayURL: "http://relay:8081"}
	if mutate != nil {
		mutate(&cfg)
	}
	d, err := New(cfg, srv.URL, nil, func() (string, error) { return "sa-token", nil })
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCreateBuildsHardenedPodAndStoresInputInSecret(t *testing.T) {
	api := newFakeAPI()
	d := newDriver(t, api, func(c *Config) {
		c.RuntimeClassName = "gvisor"
		c.EnvFromSecrets = []string{"git-creds"}
		c.CPULimit = "2"
	})
	input := []byte(`{"task":{"auth_token":"mat_x"}}`)
	if err := d.Create(context.Background(), controller.PodSpec{Name: "multica-task-t1", Annotations: map[string]string{"multica.ai/task-id": "t1"}, Input: input}); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(api.pods["multica-task-t1"])
	pod := string(raw)
	for _, want := range []string{
		`"automountServiceAccountToken":false`,
		`"runtimeClassName":"gvisor"`,
		`"runAsNonRoot":true`,
		`"allowPrivilegeEscalation":false`,
		`"drop":["ALL"]`,
		`"restartPolicy":"Never"`,
		`"secretName":"multica-task-t1"`,
		`"name":"git-creds"`,
		`"cpu":"2"`,
		`"value":"http://relay:8081"`, // MULTICA_SERVER_URL points at the relay
	} {
		if !strings.Contains(pod, want) {
			t.Errorf("pod manifest is missing %s\n%s", want, pod)
		}
	}
	if strings.Contains(pod, "mat_x") {
		t.Error("the task token must live only in the Secret, never in the Pod spec")
	}

	secret, _ := json.Marshal(api.secrets["multica-task-t1"])
	if !strings.Contains(string(secret), base64.StdEncoding.EncodeToString(input)) {
		t.Errorf("input not stored in secret: %s", secret)
	}
	if api.auth != "Bearer sa-token" {
		t.Errorf("service account token not sent: %q", api.auth)
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	api := newFakeAPI()
	d := newDriver(t, api, nil)
	spec := controller.PodSpec{Name: "multica-task-t1", Input: []byte("{}")}
	for i := 0; i < 2; i++ {
		if err := d.Create(context.Background(), spec); err != nil {
			t.Fatalf("create #%d: %v", i+1, err)
		}
	}
	if len(api.pods) != 1 || len(api.secrets) != 1 {
		t.Fatalf("pods=%d secrets=%d", len(api.pods), len(api.secrets))
	}
}

func TestListMapsPhaseAndReason(t *testing.T) {
	api := newFakeAPI()
	d := newDriver(t, api, nil)
	_ = d.Create(context.Background(), controller.PodSpec{Name: "multica-task-t1", Annotations: map[string]string{"multica.ai/task-id": "t1"}, Input: []byte("{}")})
	api.pods["multica-task-t1"]["__phase"] = "Failed"

	pods, err := d.List(context.Background())
	if err != nil || len(pods) != 1 {
		t.Fatalf("list = %v, %v", pods, err)
	}
	p := pods[0]
	if p.Phase != controller.PhaseFailed || p.Annotations["multica.ai/task-id"] != "t1" || !strings.Contains(p.Reason, "OOMKilled") {
		t.Fatalf("pod = %+v", p)
	}
	if p.CreatedAt.IsZero() {
		t.Fatal("creation time not parsed")
	}
}

func TestInputRoundTripAndDelete(t *testing.T) {
	api := newFakeAPI()
	d := newDriver(t, api, nil)
	want := []byte(`{"a":1}`)
	_ = d.Create(context.Background(), controller.PodSpec{Name: "multica-task-t1", Input: want})
	got, err := d.Input(context.Background(), "multica-task-t1")
	if err != nil || string(got) != string(want) {
		t.Fatalf("input = %q, %v", got, err)
	}
	if err := d.Delete(context.Background(), "multica-task-t1"); err != nil {
		t.Fatal(err)
	}
	if len(api.pods)+len(api.secrets) != 0 {
		t.Fatal("pod and secret must both be deleted")
	}
	if err := d.Delete(context.Background(), "multica-task-t1"); err != nil {
		t.Fatalf("deleting a missing pod must succeed: %v", err)
	}
}

func TestNewRequiresCoreSettings(t *testing.T) {
	if _, err := New(Config{}, "http://x", nil, nil); err == nil {
		t.Fatal("missing settings must be rejected")
	}
}

func TestCapacityErrorsAreClassified(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"exceeded quota", 403, `{"message":"pods \"x\" is forbidden: exceeded quota: podq, requested: pods=1, used: pods=3, limited: pods=3"}`, true},
		{"missing RBAC is permanent", 403, `{"message":"pods is forbidden: User cannot create resource"}`, false},
		{"throttled", 429, "", true},
		{"api unavailable", 503, "", true},
		{"invalid spec is permanent", 422, `{"message":"Invalid value"}`, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/secrets") {
				w.WriteHeader(http.StatusCreated)
				return
			}
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		d, _ := New(Config{Namespace: "ns", DaemonID: "d1", Image: "i", RelayURL: "http://r"}, srv.URL, nil, nil)
		err := d.Create(context.Background(), controller.PodSpec{Name: "p", Input: []byte("{}")})
		srv.Close()
		if got := errors.Is(err, controller.ErrNoCapacity); got != c.want {
			t.Errorf("%s: ErrNoCapacity=%v, want %v (err=%v)", c.name, got, c.want, err)
		}
	}
}
