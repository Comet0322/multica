package gitgateway_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

const (
	pushPath     = "/group/proj.git/git-receive-pack"
	pushRefsPath = "/group/proj.git/info/refs?service=git-receive-pack"
	zeroSHA      = "0000000000000000000000000000000000000000"
	oldSHA       = "1111111111111111111111111111111111111111"
	newSHA       = "2222222222222222222222222222222222222222"
)

// pushBody is the head of a receive-pack request: one command per ref, the
// first carrying capabilities, a flush, then pack bytes.
func pushBody(pack string, refs ...string) string {
	var b strings.Builder
	for i, ref := range refs {
		line := oldSHA + " " + newSHA + " " + ref
		if i == 0 {
			line += "\x00report-status side-band-64k agent=git/2.45\n"
		} else {
			line += "\n"
		}
		b.WriteString(pkt(line))
	}
	b.WriteString("0000")
	b.WriteString(pack)
	return b.String()
}

func pushHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelDeveloper)
	return h, h.issue("t1", "alice@example.com")
}

func TestPushInsideTaskNamespaceReachesGitLabUnchanged(t *testing.T) {
	h, tok := pushHarness(t)
	body := pushBody("PACK-bytes", "refs/heads/agent/t1/fix-login", "refs/heads/agent/t1/nested/two")

	code, _ := h.do("POST", pushPath, tok, body)
	if code != 200 {
		t.Fatalf("push = %d, want 200", code)
	}
	got := h.gitlab.pushes()
	if len(got) != 1 || string(got[0].Head) != body {
		t.Fatalf("GitLab received %+v, want the request unchanged", got)
	}
	if code, _ := h.do("GET", pushRefsPath, tok, ""); code != 200 {
		t.Fatalf("receive-pack advertisement = %d, want 200", code)
	}
}

func TestPushWithAnyRefOutsideNamespaceIsRefusedWholesale(t *testing.T) {
	refs := map[string]string{
		"default branch":      "refs/heads/main",
		"another branch":      "refs/heads/feature",
		"tag":                 "refs/tags/v1",
		"other task":          "refs/heads/agent/t2/x",
		"namespace itself":    "refs/heads/agent/t1",
		"namespace slash":     "refs/heads/agent/t1/",
		"prefix lookalike":    "refs/heads/agent/t1x/y",
		"case change":         "refs/heads/agent/T1/y",
		"dot dot escape":      "refs/heads/agent/t1/../../main",
		"double slash":        "refs/heads/agent/t1//x",
		"lock suffix":         "refs/heads/agent/t1/x.lock",
		"special character":   "refs/heads/agent/t1/a~b",
		"head not branch":     "HEAD",
		"remote ref":          "refs/remotes/origin/agent/t1/x",
		"notes":               "refs/notes/commits",
		"merge request refs":  "refs/merge-requests/1/head",
		"namespace not heads": "agent/t1/x",
	}
	for name, ref := range refs {
		t.Run(name, func(t *testing.T) {
			h, tok := pushHarness(t)
			code, out := h.do("POST", pushPath, tok, pushBody("PACK", ref))
			if code == 200 && !strings.Contains(out, "ng "+ref) {
				t.Fatalf("push of %q = 200 %q, want a rejection", ref, out)
			}
			if code != 200 && code != 403 {
				t.Fatalf("push of %q = %d", ref, code)
			}
			if n := len(h.gitlab.gitRequests()); n != 0 {
				t.Fatalf("refused push reached GitLab (%d requests)", n)
			}
		})
	}

	t.Run("one bad ref among good ones", func(t *testing.T) {
		h, tok := pushHarness(t)
		body := pushBody("PACK", "refs/heads/agent/t1/a", "refs/heads/main", "refs/heads/agent/t1/b")
		_, out := h.do("POST", pushPath, tok, body)
		if !strings.Contains(out, "refs/heads/main") || len(h.gitlab.gitRequests()) != 0 {
			t.Fatalf("mixed push was not refused as a whole: %q, %d GitLab requests", out, len(h.gitlab.gitRequests()))
		}
	})

	t.Run("delete outside namespace", func(t *testing.T) {
		h, tok := pushHarness(t)
		body := pkt(oldSHA+" "+zeroSHA+" refs/heads/main\x00report-status\n") + "0000"
		h.do("POST", pushPath, tok, body)
		if len(h.gitlab.gitRequests()) != 0 {
			t.Fatal("deleting the default branch reached GitLab")
		}
	})

	t.Run("delete inside namespace", func(t *testing.T) {
		h, tok := pushHarness(t)
		body := pkt(oldSHA+" "+zeroSHA+" refs/heads/agent/t1/old\x00report-status\n") + "0000"
		if code, _ := h.do("POST", pushPath, tok, body); code != 200 || len(h.gitlab.pushes()) != 1 {
			t.Fatalf("deleting the task's own branch = %d, pushes %d", code, len(h.gitlab.pushes()))
		}
	})
}

func TestMalformedPushRequestsAreRefused(t *testing.T) {
	cmd := func(s string) string { return pkt(s) + "0000" }
	cases := map[string]string{
		"not pkt-lines":      "hello world, this is not git",
		"short body":         "00",
		"length too small":   "0002" + "0000",
		"length not hex":     "zzzz" + oldSHA,
		"signed length":      "+010" + oldSHA,
		"truncated command":  "0050" + oldSHA + " " + newSHA,
		"no flush":           pkt(oldSHA + " " + newSHA + " refs/heads/agent/t1/x\x00report-status\n"),
		"bad sha":            cmd("xyz " + newSHA + " refs/heads/agent/t1/x\x00report-status\n"),
		"mismatched sha":     cmd(oldSHA + " " + newSHA[:30] + " refs/heads/agent/t1/x\x00report-status\n"),
		"missing ref":        cmd(oldSHA + " " + newSHA + "\x00report-status\n"),
		"NUL in later line":  pkt(oldSHA+" "+newSHA+" refs/heads/agent/t1/a\x00report-status\n") + pkt(oldSHA+" "+newSHA+" refs/heads/main\x00x\n") + "0000",
		"push certificate":   cmd("push-cert\x00report-status\n"),
		"data after bare 0":  "0000PACK",
		"oversized pkt":      "fff0" + strings.Repeat("a", 100),
		"command list > 1MB": strings.Repeat(pkt(oldSHA+" "+newSHA+" refs/heads/agent/t1/"+strings.Repeat("a", 60000)+"\n"), 20) + "0000",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h, tok := pushHarness(t)
			code, _ := h.do("POST", pushPath, tok, body)
			if code == 200 {
				t.Fatalf("malformed push was accepted")
			}
			if n := len(h.gitlab.gitRequests()); n != 0 {
				t.Fatalf("malformed push reached GitLab (%d requests)", n)
			}
		})
	}
}

func TestBareFlushProbeAndShallowLinesPass(t *testing.T) {
	h, tok := pushHarness(t)
	// git probes large pushes with a lone flush before streaming the pack.
	if code, _ := h.do("POST", pushPath, tok, "0000"); code != 200 {
		t.Fatalf("probe = %d, want 200", code)
	}
	body := pkt("shallow "+oldSHA+"\n") + pushBody("PACK", "refs/heads/agent/t1/x")
	if code, _ := h.do("POST", pushPath, tok, body); code != 200 {
		t.Fatalf("push from a shallow clone = %d, want 200", code)
	}
}

func TestPushRefusalIsReadableByGit(t *testing.T) {
	h, tok := pushHarness(t)
	_, out := h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/main"))
	for _, want := range []string{"unpack ok", "ng refs/heads/main", "outside the task branch namespace refs/heads/agent/t1/*"} {
		if !strings.Contains(out, want) {
			t.Errorf("response lacks %q: %q", want, out)
		}
	}
	// With side-band the reason is also shown as "remote: ...".
	if !strings.Contains(out, "\x02git gateway:") {
		t.Errorf("no sideband message: %q", out)
	}

	// Refusing the ref advertisement shows up as "remote error:" in git.
	reporter := h.issue("t3", "bob@example.com")
	h.gitlab.addUser("bob@example.com", 8, "active", gitgateway.LevelReporter)
	code, out := h.do("GET", pushRefsPath, reporter, "")
	if code != 200 || !strings.Contains(out, "ERR git gateway: access level 20 is below the 30 needed to push") {
		t.Errorf("advertisement refusal = %d %q", code, out)
	}
}

func TestPushNeedsDeveloperCheckedLiveOnEveryPush(t *testing.T) {
	h, tok := pushHarness(t)
	body := pushBody("PACK", "refs/heads/agent/t1/x")
	if code, _ := h.do("POST", pushPath, tok, body); code != 200 {
		t.Fatalf("first push = %d", code)
	}
	// Downgraded between pushes: the second is refused although the cache
	// (used for reads) still says Developer.
	h.do("GET", refsPath, tok, "")
	h.gitlab.setLevel(7, gitgateway.LevelReporter)
	_, out := h.do("POST", pushPath, tok, body)
	if !strings.Contains(out, "ng refs/heads/agent/t1/x") || len(h.gitlab.pushes()) != 1 {
		t.Fatalf("second push after downgrade: %q, pushes %d", out, len(h.gitlab.pushes()))
	}
}

func TestPushRefusedWhenGitLabLookupFailsOrUserUnknown(t *testing.T) {
	h, tok := pushHarness(t)
	h.gitlab.setFail(500)
	code, _ := h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
	if code != http.StatusServiceUnavailable || len(h.gitlab.pushes()) != 0 {
		t.Fatalf("push while GitLab fails = %d, pushes %d", code, len(h.gitlab.pushes()))
	}
	h.gitlab.setFail(0)
	ghost := h.issue("t9", "ghost@example.com")
	if _, out := h.do("POST", pushPath, ghost, pushBody("PACK", "refs/heads/agent/t9/x")); !strings.Contains(out, "no gitlab user") {
		t.Fatalf("unknown initiator: %q", out)
	}
	none := h.issue("t10", "")
	if _, out := h.do("POST", pushPath, none, pushBody("PACK", "refs/heads/agent/t10/x")); !strings.Contains(out, "no human initiator") {
		t.Fatalf("no initiator: %q", out)
	}
	if code, _ := h.do("POST", pushPath, "", pushBody("PACK", "refs/heads/agent/t1/x")); code != 401 {
		t.Fatalf("push without a token = %d, want 401", code)
	}
	if len(h.gitlab.pushes()) != 0 {
		t.Fatal("a refused push reached GitLab")
	}
}

func TestTaskIdsThatCannotFormANamespaceCannotPush(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelDeveloper)
	for _, id := range []string{"a b", "../x", "x..y", "a~b", "x.lock"} {
		tok := h.issue(id, "alice@example.com")
		h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/"+id+"/x"))
	}
	if n := len(h.gitlab.gitRequests()); n != 0 {
		t.Fatalf("%d pushes reached GitLab", n)
	}
}

func TestPushAuditRecordsRefsAndNoCredentials(t *testing.T) {
	h, tok := pushHarness(t)
	h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
	h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/main"))

	recs := h.audit.records(t)
	if len(recs) != 2 {
		t.Fatalf("got %d audit records: %s", len(recs), h.audit.String())
	}
	if r := recs[0]; r.Decision != gitgateway.AuditAllow || r.Action != gitgateway.ActionReceivePack ||
		r.TaskID != "t1" || r.Initiator != "alice@example.com" || r.Project != project ||
		len(r.Refs) != 1 || r.Refs[0] != "refs/heads/agent/t1/x" {
		t.Fatalf("allow record = %+v", r)
	}
	if r := recs[1]; r.Decision != gitgateway.AuditDeny || !strings.Contains(r.Reason, "outside the task branch namespace") ||
		len(r.Refs) != 1 || r.Refs[0] != "refs/heads/main" {
		t.Fatalf("deny record = %+v", r)
	}
	for _, sealed := range []string{tok, adminTicket} {
		if strings.Contains(h.audit.String(), sealed) {
			t.Fatalf("audit log contains a credential")
		}
	}
}

func TestCustomBranchPrefix(t *testing.T) {
	h := newHarness(t, func(c *gitgateway.Config) { c.BranchPrefix = "bots/" })
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelDeveloper)
	tok := h.issue("t1", "alice@example.com")
	h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
	if len(h.gitlab.pushes()) != 0 {
		t.Fatal("default prefix accepted although BranchPrefix is set")
	}
	h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/bots/t1/x"))
	if len(h.gitlab.pushes()) != 1 {
		t.Fatal("configured prefix refused")
	}
}

// A 50 MB pack must flow through without the gateway holding it.
func TestLargePushIsStreamedNotBuffered(t *testing.T) {
	h, tok := pushHarness(t)
	const packSize = 50 << 20

	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte(pushBody("", "refs/heads/agent/t1/big")))
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for sent := 0; sent < packSize; sent += len(chunk) {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		pw.Close()
	}()

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	req, _ := http.NewRequest("POST", h.gateway.URL+pushPath, pr)
	req.SetBasicAuth("task", tok)
	resp, err := h.gateway.Client().Do(req)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := h.gitlab.pushes()
	if resp.StatusCode != 200 || len(got) != 1 || got[0].Size < packSize {
		t.Fatalf("push = %d, GitLab saw %+v", resp.StatusCode, got)
	}
	if grew := int64(peak.Load()) - int64(base.HeapAlloc); grew > 10<<20 {
		t.Fatalf("heap grew by %d MB during a %d MB push, want at most 10", grew>>20, packSize>>20)
	}
}

// With a real git client: a push outside the namespace is stopped by the
// gateway with a message git prints, and nothing reaches GitLab.
func TestRealGitClientSeesWhyPushWasRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h, tok := pushHarness(t)
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "HOME="+dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	run("add", "f")
	run("commit", "-q", "-m", "c")
	remote := "http://task:" + tok + "@" + strings.TrimPrefix(h.gateway.URL, "http://") + "/group/proj.git"

	out := run("push", remote, "HEAD:refs/heads/main")
	if !strings.Contains(out, "outside the task branch namespace") || !strings.Contains(out, "rejected") {
		t.Fatalf("git output does not explain the refusal: %q", out)
	}
	if n := len(h.gitlab.gitRequests()); n != 1 { // only the ref advertisement
		t.Fatalf("GitLab saw %d git requests, want only the advertisement", n)
	}

	reporter := h.issue("t5", "bob@example.com")
	h.gitlab.addUser("bob@example.com", 8, "active", gitgateway.LevelReporter)
	out = run("push", "http://task:"+reporter+"@"+strings.TrimPrefix(h.gateway.URL, "http://")+"/group/proj.git", "HEAD:refs/heads/agent/t5/x")
	if !strings.Contains(out, "below the 30 needed to push") {
		t.Fatalf("git output does not explain the permission refusal: %q", out)
	}

	before := len(h.gitlab.pushes())
	run("push", remote, fmt.Sprintf("HEAD:refs/heads/agent/%s/x", "t1"))
	if len(h.gitlab.pushes()) != before+1 {
		t.Fatal("a legitimate push did not reach GitLab")
	}
}
