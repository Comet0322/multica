package gitgateway

import (
	"bufio"
	"bytes"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxReportBytes bounds how much of GitLab's receive-pack answer is kept to
// read the per-ref result. The report is a few lines; the cap only guards
// against a misbehaving upstream.
const maxReportBytes = 1 << 20

// Defaults bounding the push log. A task's record is dropped when its token
// is revoked (when the gateway can be told); these limits catch the rest, such
// as a gateway running apart from the manager.
const (
	defaultPushLogTTL      = 24 * time.Hour
	defaultPushLogMaxTasks = 10000
)

// pushLog remembers, per task, which branches that task pushed successfully.
// Only a ref GitLab reported as updated is recorded, and a task never sees
// another task's entries. It is bounded: a task's record goes when the task
// is forgotten, when it was last written longer ago than ttl, or when more
// than maxTasks tasks are held (the least recently written go first).
type pushLog struct {
	mu       sync.Mutex
	now      func() time.Time
	ttl      time.Duration
	maxTasks int
	branches map[string]map[string]map[string]struct{} // task -> project -> branch
	written  map[string]time.Time                      // task -> last write
}

func newPushLog(now func() time.Time, ttl time.Duration, maxTasks int) *pushLog {
	if ttl <= 0 {
		ttl = defaultPushLogTTL
	}
	if maxTasks <= 0 {
		maxTasks = defaultPushLogMaxTasks
	}
	return &pushLog{now: now, ttl: ttl, maxTasks: maxTasks}
}

// forget drops everything recorded for the task.
func (l *pushLog) forget(task string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.branches, task)
	delete(l.written, task)
}

// pruneLocked applies the age and size limits.
func (l *pushLog) pruneLocked() {
	now := l.now()
	for task, at := range l.written {
		if now.Sub(at) > l.ttl {
			delete(l.branches, task)
			delete(l.written, task)
		}
	}
	for len(l.written) > l.maxTasks {
		var oldest string
		var oldestAt time.Time
		for task, at := range l.written {
			if oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = task, at
			}
		}
		delete(l.branches, oldest)
		delete(l.written, oldest)
	}
}

func (l *pushLog) add(task, project, branch string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.branches == nil {
		l.branches = map[string]map[string]map[string]struct{}{}
		l.written = map[string]time.Time{}
	}
	l.written[task] = l.now()
	l.pruneLocked()
	if l.branches[task] == nil {
		l.branches[task] = map[string]map[string]struct{}{}
	}
	if l.branches[task][project] == nil {
		l.branches[task][project] = map[string]struct{}{}
	}
	l.branches[task][project][branch] = struct{}{}
}

func (l *pushLog) remove(task, project, branch string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.branches[task][project], branch)
}

func (l *pushLog) list(task, project string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked()
	out := make([]string, 0, len(l.branches[task][project]))
	for b := range l.branches[task][project] {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// PushedBranches lists the branches (names without refs/heads/) the task has
// successfully pushed to the project through this gateway, sorted. Rejected
// or failed pushes leave no entry, and entries of other tasks are never
// returned.
func (g *Gateway) PushedBranches(taskID, project string) []string {
	return g.pushed.list(taskID, project)
}

// recordPush notes the refs GitLab accepted. A branch the task deleted is
// forgotten again.
func (g *Gateway) recordPush(taskID, project string, cmds []pushCommand, report []byte) {
	ok := parseReport(report)
	for _, c := range cmds {
		branch, isBranch := strings.CutPrefix(c.Ref, "refs/heads/")
		if !isBranch || !ok[c.Ref] {
			continue
		}
		if strings.Trim(c.New, "0") == "" {
			g.pushed.remove(taskID, project, branch)
		} else {
			g.pushed.add(taskID, project, branch)
		}
	}
}

// parseReport reads a receive-pack report-status answer, with or without
// side-band framing, and returns the refs reported "ok". Nothing is accepted
// unless the pack unpacked.
func parseReport(body []byte) map[string]bool {
	br := bufio.NewReader(bytes.NewReader(body))
	var inner, scratch bytes.Buffer
	var lines [][]byte
	for {
		scratch.Reset()
		payload, flush, err := readPkt(br, &scratch)
		if err != nil {
			break
		}
		if flush || len(payload) == 0 {
			continue
		}
		switch payload[0] {
		case 1:
			inner.Write(payload[1:])
		case 2, 3:
		default:
			lines = append(lines, payload)
		}
	}
	ib := bufio.NewReader(&inner)
	for {
		scratch.Reset()
		payload, flush, err := readPkt(ib, &scratch)
		if err != nil {
			break
		}
		if !flush {
			lines = append(lines, payload)
		}
	}
	ok, unpacked := map[string]bool{}, false
	for _, p := range lines {
		line := strings.TrimSuffix(string(p), "\n")
		switch {
		case line == "unpack ok":
			unpacked = true
		case strings.HasPrefix(line, "ok "):
			ok[strings.TrimPrefix(line, "ok ")] = true
		}
	}
	if !unpacked {
		return nil
	}
	return ok
}

// reportRecorder passes GitLab's answer through untouched while keeping the
// head of it, so the gateway can read which refs were updated.
type reportRecorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *reportRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *reportRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if room := maxReportBytes - r.buf.Len(); room > 0 {
		r.buf.Write(p[:min(len(p), room)])
	}
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the real writer to flush.
func (r *reportRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
