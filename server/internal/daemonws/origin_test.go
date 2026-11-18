package daemonws

import (
	"net/http/httptest"
	"testing"
)

func TestHubRefusesBrowserOrigins(t *testing.T) {
	check := NewHub().upgrader.CheckOrigin

	native := httptest.NewRequest("GET", "/api/daemon/ws", nil)
	if !check(native) {
		t.Fatal("a daemon request without Origin was refused")
	}

	browser := httptest.NewRequest("GET", "/api/daemon/ws", nil)
	browser.Header.Set("Origin", "https://example.com")
	if check(browser) {
		t.Fatal("a browser request with an Origin was accepted")
	}
}
