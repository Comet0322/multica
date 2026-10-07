package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestWriteExtDecisionResult(t *testing.T) {
	run := ExtWorkflowRunResponse{}
	for name, tc := range map[string]struct {
		found bool
		err   error
		want  int
	}{
		"reloaded":      {found: true, want: http.StatusOK},
		"reload failed": {err: errors.New("db down"), want: http.StatusNoContent},
		"run gone":      {found: false, want: http.StatusNoContent},
	} {
		rec := httptest.NewRecorder()
		writeExtDecisionResult(rec, pgtype.UUID{}, run, tc.found, tc.err)
		if rec.Code != tc.want {
			t.Fatalf("%s: status = %d, want %d", name, rec.Code, tc.want)
		}
		if tc.want == http.StatusNoContent && rec.Body.Len() != 0 {
			t.Fatalf("%s: 204 carries a body: %q", name, rec.Body.String())
		}
	}
}
