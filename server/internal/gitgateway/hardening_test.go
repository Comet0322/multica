package gitgateway_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

func TestUnclosedCommentInTheDescriptionCannotSwallowTheFooter(t *testing.T) {
	m := newMRHarness(t, gitgateway.LevelDeveloper)
	m.push("refs/heads/agent/t1/fix")
	for _, opening := range []string{"<!--", "text <!-- never closed", "<<!--", "<!-- a --><!--"} {
		if _, isErr := m.createMR(map[string]any{"source_branch": "agent/t1/fix", "title": "T", "description": opening}); isErr {
			t.Fatalf("%q: create refused", opening)
		}
	}
	for i, mr := range m.gitlab.createdMRs() {
		desc, _ := mr["description"].(string)
		// The only comment opener left is the gateway's own, and the footer is
		// the last thing in the description.
		const mark = "<!-- multica-git-gateway -->"
		if strings.Count(desc, "<!--") != 1 || strings.Index(desc, "<!--") != strings.Index(desc, mark) {
			t.Errorf("description %d still holds an agent comment opener:\n%s", i, desc)
		}
		if !strings.HasSuffix(desc, "- Label: `created-by-agent`\n") || !strings.Contains(desc[strings.Index(desc, mark):], "alice@example.com") {
			t.Errorf("description %d does not end with the gateway footer:\n%s", i, desc)
		}
	}
}

func TestGitLabErrorBodyEchoingASecretIsRedacted(t *testing.T) {
	logs := &syncBuffer{}
	m := newMRHarness(t, gitgateway.LevelDeveloper, func(c *gitgateway.Config) {
		c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	m.push("refs/heads/agent/t1/fix")
	m.gitlab.refuseMRsWith(`{"message":["duplicate for ` + adminToken + `"]}`)

	text, isErr := m.createMR(map[string]any{"source_branch": "agent/t1/fix", "title": "T"})
	if !isErr {
		t.Fatalf("create succeeded: %q", text)
	}
	for name, got := range map[string]string{"tool result": text, "log": logs.String(), "audit": m.audit.String()} {
		if strings.Contains(got, adminToken) {
			t.Errorf("%s echoes the admin token: %s", name, got)
		}
	}
	if !strings.Contains(logs.String(), "refused the merge request") {
		t.Errorf("the refusal itself should still be logged: %s", logs.String())
	}
}

func TestResolversReadAtMostABoundedReply(t *testing.T) {
	// A valid reply that is far larger than any identity must not be read in
	// full.
	big := strings.Repeat("x", 8<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id": "t1", "initiator": big,
			"identities": []gitgateway.Identity{{TaskID: "t1", Initiator: big}},
		})
	}))
	t.Cleanup(srv.Close)

	if _, err := (gitgateway.HTTPResolver{URL: srv.URL}).Resolve(context.Background(), "tok"); err == nil {
		t.Error("HTTPResolver accepted an oversized reply")
	}
	if _, err := (gitgateway.HTTPPodResolver{URL: srv.URL}).ResolvePod(context.Background(), "10.0.0.1"); err == nil {
		t.Error("HTTPPodResolver accepted an oversized reply")
	}
}
