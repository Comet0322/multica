package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

func TestLoadPoolReadsTheGatewayListener(t *testing.T) {
	env := baseEnv()
	env["SANDBOX_GIT_GATEWAY_URL"] = "http://git-gateway:8081"
	env["SANDBOX_POOL_GATEWAY_LISTEN"] = ":8792"
	s, err := loadPool(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if s.gatewayListen != ":8792" {
		t.Fatalf("gateway listener = %q", s.gatewayListen)
	}
	delete(env, "SANDBOX_GIT_GATEWAY_URL")
	if _, err := loadPool(envLookup(env)); err == nil || !strings.Contains(err.Error(), "SANDBOX_POOL_GATEWAY_LISTEN") {
		t.Fatalf("a gateway listener without a git gateway must be refused, got %v", err)
	}
}

func TestGatewayListenerCarriesOnlyTheVerifyEndpoints(t *testing.T) {
	env := baseEnv()
	env["SANDBOX_GIT_GATEWAY_URL"] = "http://git-gateway:8081"
	s, err := loadPool(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	s.config.MulticaURL = newFakeMultica(t, testWorkspaceA).URL
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := sandboxmanager.NewPool(s.config, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	podLn, _ := net.Listen("tcp", "127.0.0.1:0")
	adminLn, _ := net.Listen("tcp", "127.0.0.1:0")
	gatewayLn, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- servePool(ctx, log, pool, podLn, adminLn, gatewayLn, make(chan os.Signal)) }()
	defer func() {
		cancel()
		<-done
	}()

	get := func(ln net.Listener, p string) int {
		t.Helper()
		resp, err := http.Get("http://" + ln.Addr().String() + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// A wrong method tells a mounted endpoint (405) from a missing one (404).
	if got := get(gatewayLn, "/v1/git/verify-token"); got != http.StatusMethodNotAllowed {
		t.Errorf("the gateway listener must serve verify-token, got %d", got)
	}
	if got := get(gatewayLn, "/v1/inventory"); got != http.StatusNotFound {
		t.Errorf("the gateway listener must not serve the inventory, got %d", got)
	}
	if got := get(podLn, "/v1/git/verify-token"); got == http.StatusMethodNotAllowed || got == http.StatusOK {
		t.Errorf("the pod listener served a verify endpoint: %d", got)
	}
}
