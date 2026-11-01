package sandboxmanager_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayHandlerServesOnlyTheVerifyEndpoints(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withGateway())
	srv := httptest.NewServer(h.pool.GatewayHandler())
	t.Cleanup(srv.Close)

	post := func(path, body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("/v1/git/verify-pod", `{"ip":"10.0.0.9"}`); got != http.StatusOK {
		t.Errorf("verify-pod = %d, want 200", got)
	}
	if got := post("/v1/git/verify-token", `{"token":"gwt_unknown"}`); got != http.StatusNotFound {
		t.Errorf("verify-token for an unknown token = %d, want 404", got)
	}
	// Nothing else of the admin or pod API is on this handler.
	for _, path := range []string{"/v1/inventory", "/v1/leases", "/pod/control", "/api/daemon/register", "/healthz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestGatewayHandlerIsEmptyWithoutAGitGateway(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	srv := httptest.NewServer(h.pool.GatewayHandler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/v1/git/verify-pod", "application/json", strings.NewReader(`{"ip":"10.0.0.9"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("verify-pod without a git gateway = %d, want 404", resp.StatusCode)
	}
}
