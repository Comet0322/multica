package conformance

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/multica-ai/multica/server/internal/sandboxprovider"
)

// TestDockerProviderConformance runs the suite against real Docker through
// the reference HTTP handler. Opt in with a built probe image:
//
//	docker build -t multica-sandbox-probe deploy/sandbox/conformance
//	SANDBOX_CONFORMANCE_PROBE_IMAGE=multica-sandbox-probe go test ./internal/sandboxprovider/conformance -run Docker -v
func TestDockerProviderConformance(t *testing.T) {
	image := os.Getenv("SANDBOX_CONFORMANCE_PROBE_IMAGE")
	if image == "" {
		t.Skip("set SANDBOX_CONFORMANCE_PROBE_IMAGE to run against Docker")
	}
	docker, err := sandboxprovider.NewDocker(sandboxprovider.DockerConfig{
		Instance:   "conformance",
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	collector := NewCollector()
	go func() { _ = http.Serve(ln, collector) }()
	defer ln.Close()
	service := httptest.NewServer(sandboxprovider.NewHTTPHandler(docker, "token"))
	defer service.Close()
	provider, _ := sandboxprovider.NewHTTP(sandboxprovider.HTTPConfig{BaseURL: service.URL, Token: "token"})

	ctx := context.Background()
	results := CheckHTTPAPI(ctx, service.URL, "token")
	results = append(results, Run(ctx, provider, collector, Config{
		Image:       image,
		CallbackURL: fmt.Sprintf("http://host.docker.internal:%d", ln.Addr().(*net.TCPAddr).Port),
	})...)
	for _, r := range results {
		t.Logf("%s %s %s", r.Level, r.Name, r.Message)
	}
	if Failed(results) {
		t.Fatal("Docker provider failed conformance")
	}
}
