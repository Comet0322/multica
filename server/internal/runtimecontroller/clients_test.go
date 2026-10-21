package runtimecontroller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func testCfg() Config {
	return Config{
		Namespace: "mr", Image: "mr/daemon:test", ImagePullPolicy: "Never", PVCSize: "2Gi",
		CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "1Gi",
		PodServerURL: "http://server.mr:8080", WorkspacesRoot: "/data/workspaces", DrainTimeout: 10 * time.Minute,
	}
}

func nested(t *testing.T, u *unstructured.Unstructured, path ...string) any {
	t.Helper()
	v, found, err := unstructured.NestedFieldCopy(u.Object, path...)
	if err != nil || !found {
		t.Fatalf("missing %v (err=%v)", path, err)
	}
	return v
}

func TestBuildSandboxIsHardenedAndUsesTokenFile(t *testing.T) {
	u := BuildSandbox(testCfg(), SandboxSpec{Name: "mr-abc", WorkspaceID: "ws1", DaemonID: "mr-abc", LLM: true})
	pod := []string{"spec", "podTemplate", "spec"}
	at := func(extra ...string) any { return nested(t, u, append(append([]string{}, pod...), extra...)...) }

	if at("automountServiceAccountToken") != false || at("enableServiceLinks") != false {
		t.Fatal("pod must not get a service-account token or service links")
	}
	sc := at("securityContext").(map[string]any)
	if sc["runAsNonRoot"] != true || sc["runAsUser"].(int64) == 0 {
		t.Fatalf("pod must run as non-root: %v", sc)
	}
	c := at("containers").([]any)[0].(map[string]any)
	cmd := strings.Join(toStrings(c["command"].([]any)), " ")
	for _, want := range []string{"daemon start", "--foreground", "--managed", "--token-file=/var/run/multica/token"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q missing %q", cmd, want)
		}
	}
	csc := c["securityContext"].(map[string]any)
	if csc["allowPrivilegeEscalation"] != false {
		t.Error("privilege escalation must be off")
	}
	if envFrom, ok := c["envFrom"].([]any); !ok || len(envFrom) != 1 {
		t.Errorf("LLM secret not wired: %v", c["envFrom"])
	}
	grace := at("terminationGracePeriodSeconds").(int64)
	if grace != 10*60+30 {
		t.Errorf("grace period %d must exceed the drain timeout", grace)
	}
	// The token comes from a mounted Secret, never from an env var.
	env := toStrings(envNames(c["env"].([]any)))
	for _, e := range env {
		if strings.Contains(e, "TOKEN") {
			t.Errorf("env var %s must not carry credentials", e)
		}
	}
	if _, found, _ := unstructured.NestedString(u.Object, "spec", "podTemplate", "spec", "runtimeClassName"); found {
		t.Error("runtimeClassName set without configuration")
	}
	labels := nested(t, u, "metadata", "labels").(map[string]any)
	if labels[LabelWorkspaceID] != "ws1" || labels[LabelDaemonID] != "mr-abc" || labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("labels: %v", labels)
	}
}

func TestBuildSandboxOptions(t *testing.T) {
	cfg := testCfg()
	cfg.RuntimeClassName = "gvisor"
	u := BuildSandbox(cfg, SandboxSpec{Name: "n", WorkspaceID: "w", DaemonID: "n"})
	if got := nested(t, u, "spec", "podTemplate", "spec", "runtimeClassName"); got != "gvisor" {
		t.Errorf("runtimeClassName=%v", got)
	}
	c := nested(t, u, "spec", "podTemplate", "spec", "containers").([]any)[0].(map[string]any)
	if _, ok := c["envFrom"]; ok {
		t.Error("envFrom must be absent without an LLM secret")
	}
}

func toStrings(in []any) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i], _ = v.(string)
	}
	return out
}

func envNames(in []any) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v.(map[string]any)["name"]
	}
	return out
}

func newKubeClient(t *testing.T) *KubeClient {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		sandboxGVR: "SandboxList",
	})
	return &KubeClient{Cfg: testCfg(), Dynamic: dyn, Core: kubefake.NewSimpleClientset()}
}

func TestKubeClientSandboxLifecycle(t *testing.T) {
	k := newKubeClient(t)
	ctx := context.Background()

	if sb, err := k.GetSandbox(ctx, "mr-abc"); err != nil || sb != nil {
		t.Fatalf("absent sandbox: %v %v", sb, err)
	}
	spec := SandboxSpec{Name: "mr-abc", WorkspaceID: "ws1", DaemonID: "mr-abc"}
	if err := k.CreateSandbox(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := k.CreateSandbox(ctx, spec); err != nil {
		t.Fatalf("create must be idempotent: %v", err)
	}
	sb, _ := k.GetSandbox(ctx, "mr-abc")
	if sb == nil || sb.Mode != ModeRunning || sb.Labels[LabelDaemonID] != "mr-abc" {
		t.Fatalf("created sandbox: %+v", sb)
	}
	if err := k.SetMode(ctx, "mr-abc", ModeSuspended); err != nil {
		t.Fatal(err)
	}
	if sb, _ := k.GetSandbox(ctx, "mr-abc"); sb.Mode != ModeSuspended {
		t.Fatalf("mode after suspend: %s", sb.Mode)
	}
	list, err := k.ListManagedSandboxes(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if err := k.DeleteSandbox(ctx, "mr-abc"); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteSandbox(ctx, "mr-abc"); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
}

func TestKubeClientSecrets(t *testing.T) {
	k := newKubeClient(t)
	ctx := context.Background()
	if s, err := k.GetSecret(ctx, "x"); err != nil || s != nil {
		t.Fatalf("absent secret: %v %v", s, err)
	}
	if err := k.PutSecret(ctx, "x", map[string]string{"a": "b"}, map[string]string{"n": "1"}, map[string]string{"token": "one"}); err != nil {
		t.Fatal(err)
	}
	if err := k.PutSecret(ctx, "x", nil, map[string]string{"n": "2"}, map[string]string{"token": "two"}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	s, _ := k.GetSecret(ctx, "x")
	if s == nil || s.Annotations["n"] != "2" {
		t.Fatalf("overwrite lost annotations: %+v", s)
	}
	if err := k.DeleteSecret(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteSecret(ctx, "x"); err != nil {
		t.Fatalf("delete idempotent: %v", err)
	}
}

func TestSandboxInfoReadyCondition(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "s"},
		"spec":     map[string]any{"operatingMode": "Suspended"},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}}
	if info := sandboxInfo(u); !info.Ready || info.Mode != ModeSuspended {
		t.Fatalf("%+v", info)
	}
	u.Object["spec"] = map[string]any{}
	if sandboxInfo(u).Mode != ModeRunning {
		t.Fatal("default mode is Running")
	}
}

func TestHTTPServerClient(t *testing.T) {
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(body))
		switch r.URL.Path {
		case "/internal/v1/managed-runtimes/state":
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(map[string]any{"runtimes": []Runtime{{DaemonID: "mr-abc", QueuedTasks: 2}}})
				return
			}
		case "/internal/v1/daemon-tokens":
			if r.Method == http.MethodPost {
				json.NewEncoder(w).Encode(map[string]any{"token": "mdt_x", "expires_at": "2026-10-23T00:00:00Z"})
				return
			}
		}
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	s := NewHTTPServer(ts.URL, "s3cret")
	ctx := context.Background()
	rts, err := s.ManagedRuntimes(ctx)
	if err != nil || len(rts) != 1 || rts[0].QueuedTasks != 2 {
		t.Fatalf("ManagedRuntimes: %v %v", rts, err)
	}
	tok, err := s.IssueDaemonToken(ctx, "ws1", "mr-abc", time.Hour)
	if err != nil || tok.Value != "mdt_x" || tok.ExpiresAt.IsZero() {
		t.Fatalf("IssueDaemonToken: %+v %v", tok, err)
	}
	if err := s.RevokeDaemonTokens(ctx, "ws1", "mr-abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportStates(ctx, []StateReport{{WorkspaceID: "ws1", DaemonID: "mr-abc", State: "running"}}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(seen, "\n")
	for _, want := range []string{`"ttl_seconds":3600`, "DELETE /internal/v1/daemon-tokens", "PUT /internal/v1/managed-runtimes/state", `"state":"running"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("requests missing %q:\n%s", want, joined)
		}
	}
	if _, err := NewHTTPServer(ts.URL, "wrong").ManagedRuntimes(ctx); err == nil {
		t.Fatal("wrong secret must error")
	}
}

func TestLiteLLMClient(t *testing.T) {
	var gen, del map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/key/generate":
			json.NewDecoder(r.Body).Decode(&gen)
			w.Write([]byte(`{"key":"sk-new"}`))
		case "/key/delete":
			json.NewDecoder(r.Body).Decode(&del)
			w.Write([]byte(`{"deleted_keys":["sk-new"]}`))
		}
	}))
	defer ts.Close()

	l := &LiteLLM{BaseURL: ts.URL, AdminKey: "admin", Models: []string{"claude-x"}, Budget: 5, RPM: 60, Client: ts.Client()}
	key, err := l.Generate(context.Background(), LLMKeyRequest{Alias: "mr-abc", WorkspaceID: "ws1", DaemonID: "mr-abc", RuntimeID: "rid"})
	if err != nil || key != "sk-new" {
		t.Fatalf("Generate: %q %v", key, err)
	}
	if gen["key_alias"] != "mr-abc" || gen["max_budget"].(float64) != 5 || gen["rpm_limit"].(float64) != 60 {
		t.Fatalf("limits not sent: %v", gen)
	}
	if md := gen["metadata"].(map[string]any); md["multica_workspace_id"] != "ws1" {
		t.Fatalf("attribution not sent: %v", gen)
	}
	if err := l.Delete(context.Background(), "sk-new"); err != nil || del["keys"].([]any)[0] != "sk-new" {
		t.Fatalf("Delete: %v %v", err, del)
	}
	bad := &LiteLLM{BaseURL: ts.URL, AdminKey: "bad", Client: ts.Client()}
	if _, err := bad.Generate(context.Background(), LLMKeyRequest{}); err == nil {
		t.Fatal("bad admin key must error")
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{ServerURL: "http://s", ControllerSecret: "x", Image: "i", TokenTTL: time.Hour, ReconcileInterval: time.Second}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no server":     func(c *Config) { c.ServerURL = "" },
		"no secret":     func(c *Config) { c.ControllerSecret = "" },
		"no image":      func(c *Config) { c.Image = "" },
		"ttl over cap":  func(c *Config) { c.TokenTTL = 48 * time.Hour },
		"ttl too small": func(c *Config) { c.TokenTTL = time.Second },
		"admin no url":  func(c *Config) { c.LiteLLMAdminKey = "k" },
	} {
		c := ok
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
