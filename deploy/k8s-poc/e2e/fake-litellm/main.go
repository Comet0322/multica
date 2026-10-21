// Fake LiteLLM admin API for the managed-runtime e2e: /key/generate,
// /key/delete and /state. It never serves a model.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sync"
)

func main() {
	admin := os.Getenv("ADMIN_KEY")
	var mu sync.Mutex
	live := map[string]map[string]any{}
	generated, deleted := 0, 0

	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+admin {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	http.HandleFunc("/key/generate", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		key := "sk-" + hex.EncodeToString(b)
		mu.Lock()
		live[key] = body
		generated++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"key": key})
	}))
	http.HandleFunc("/key/delete", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Keys []string `json:"keys"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, k := range body.Keys {
			if _, ok := live[k]; ok {
				delete(live, k)
				deleted++
			}
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted_keys": body.Keys})
	}))
	http.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"live": live, "generated": generated, "deleted": deleted})
	})
	_ = http.ListenAndServe(":4000", nil)
}
