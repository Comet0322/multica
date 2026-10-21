package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// ControllerAuth gates the internal controller API with a shared bearer
// secret. An empty secret disables the API entirely (every request is 404), so
// a deployment that does not run a runtime controller exposes nothing. The
// comparison is constant-time over fixed-length digests.
func ControllerAuth(secret string) func(http.Handler) http.Handler {
	want := sha256.Sum256([]byte(secret))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				http.NotFound(w, r)
				return
			}
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				writeError(w, http.StatusUnauthorized, "missing controller credential")
				return
			}
			got := sha256.Sum256([]byte(token))
			if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				writeError(w, http.StatusUnauthorized, "invalid controller credential")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
