package gitgateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	defaultBranchPrefix = "agent/"
	// maxCommandBytes bounds the ref update list the gateway reads before it
	// starts forwarding. The pack that follows is never buffered.
	maxCommandBytes = 1 << 20
	maxPktPayload   = 65516
)

// pushCommand is one ref update from the head of a receive-pack request.
type pushCommand struct {
	Old, New, Ref string
}

// pushRequest is a receive-pack request whose command list has been read.
// head holds the exact bytes consumed so far; head followed by rest is the
// original body.
type pushRequest struct {
	cmds []pushCommand
	caps []string
	head []byte
	rest *bufio.Reader
}

func (p *pushRequest) hasCap(name string) bool {
	for _, c := range p.caps {
		if c == name {
			return true
		}
	}
	return false
}

func (p *pushRequest) refs() []string {
	out := make([]string, len(p.cmds))
	for i, c := range p.cmds {
		out[i] = c.Ref
	}
	return out
}

// servePush handles the git-receive-pack routes. A push may only update refs
// inside the task's own branch namespace; one ref outside it refuses the whole
// push before anything reaches GitLab. Permission is asked of GitLab on every
// push, never from the cache.
func (g *Gateway) servePush(w http.ResponseWriter, r *http.Request, id Identity, rec AuditRecord, rt route) {
	namespace, why := g.branchNamespace(id.TaskID)
	if why != "" {
		rec.Reason = why
		g.denyPush(w, rec, nil, http.StatusForbidden, rt)
		return
	}

	var req *pushRequest
	if rt.action == ActionReceivePack {
		if enc := r.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
			rec.Reason = "content encoding not allowed for push"
			g.denyPush(w, rec, nil, http.StatusForbidden, rt)
			return
		}
		var why string
		if req, why = readPushRequest(r.Body); why != "" {
			rec.Reason = why
			g.denyPush(w, rec, nil, http.StatusBadRequest, rt)
			return
		}
		rec.Refs = req.refs()
		if why := checkRefs(req.cmds, namespace); why != "" {
			rec.Reason = why
			g.denyPush(w, rec, req, http.StatusForbidden, rt)
			return
		}
	}

	if status, reason := g.authorizePush(r.Context(), id.Initiator, rt.project); status != 0 {
		rec.Reason = reason
		g.denyPush(w, rec, req, status, rt)
		return
	}
	rec.Decision, rec.Time = AuditAllow, g.now().UTC()
	rec.Reason = fmt.Sprintf("initiator has at least access level %d", g.cfg.MinPushLevel)
	if req != nil {
		rec.Reason += "; refs are inside " + namespace
	}
	g.audit.write(rec)

	out := r.WithContext(context.WithValue(r.Context(), routeKey{}, rt))
	if req != nil {
		out.Body = io.NopCloser(io.MultiReader(bytes.NewReader(req.head), req.rest))
	}
	g.proxy.ServeHTTP(w, out)
}

// branchNamespace is the ref prefix the task may write to.
func (g *Gateway) branchNamespace(taskID string) (string, string) {
	if taskID == "" || !validRefComponent(taskID) {
		return "", "task id cannot form a branch namespace"
	}
	return "refs/heads/" + g.cfg.BranchPrefix + taskID + "/", ""
}

// checkRefs refuses the push when any ref is outside the namespace.
func checkRefs(cmds []pushCommand, namespace string) string {
	for _, c := range cmds {
		tail, ok := strings.CutPrefix(c.Ref, namespace)
		if !ok || tail == "" {
			return fmt.Sprintf("ref %q is outside the task branch namespace %s*", c.Ref, namespace)
		}
		if !validRefTail(tail) {
			return fmt.Sprintf("ref %q is not a valid branch name", c.Ref)
		}
	}
	return ""
}

// authorizePush returns 0 when the initiator may push to the project. Both
// the user and the access level are looked up live.
func (g *Gateway) authorizePush(ctx context.Context, email, project string) (int, string) {
	if email == "" {
		return http.StatusForbidden, "task has no human initiator"
	}
	user, err := g.gitlab.findUser(ctx, email)
	if err != nil {
		g.log.Error("git gateway user lookup failed", "error", err)
		return http.StatusServiceUnavailable, "gitlab user lookup failed"
	}
	switch {
	case !user.Found:
		return http.StatusForbidden, "no gitlab user for initiator email"
	case !user.Active:
		return http.StatusForbidden, "gitlab user is not active"
	}
	level, err := g.gitlab.accessLevel(ctx, project, user.ID)
	if err != nil {
		g.log.Error("git gateway access lookup failed", "error", err)
		return http.StatusServiceUnavailable, "gitlab access lookup failed"
	}
	if level < g.cfg.MinPushLevel {
		return http.StatusForbidden, fmt.Sprintf("access level %d is below the %d needed to push", level, g.cfg.MinPushLevel)
	}
	return 0, ""
}

// denyPush audits the refusal and answers in a form git shows to the user:
// a rejected push gets a receive-pack report, a refused ref advertisement an
// ERR line. Other failures fall back to a plain HTTP error.
func (g *Gateway) denyPush(w http.ResponseWriter, rec AuditRecord, req *pushRequest, status int, rt route) {
	rec.Decision, rec.Time = AuditDeny, g.now().UTC()
	g.audit.write(rec)
	reason := "git gateway: " + oneLine(rec.Reason)
	switch {
	case rt.action == ActionReceivePack && req != nil && status == http.StatusForbidden && (req.hasCap("report-status") || req.hasCap("report-status-v2")):
		writeRejectReport(w, req, reason)
	case rt.action == ActionPushInfoRefs && status == http.StatusForbidden:
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, pktLine("# service=git-receive-pack\n")+"0000"+pktLine("ERR "+reason+"\n"))
	default:
		http.Error(w, reason, status)
	}
}

// writeRejectReport answers a receive-pack request with every ref refused.
// git prints each as "! [remote rejected] <ref> (<reason>)", and the sideband
// message as "remote: <reason>".
func writeRejectReport(w http.ResponseWriter, req *pushRequest, reason string) {
	report := pktLine("unpack ok\n")
	for _, c := range req.cmds {
		report += pktLine("ng " + c.Ref + " " + reason + "\n")
	}
	report += "0000"
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	if req.hasCap("side-band-64k") || req.hasCap("side-band") {
		io.WriteString(w, pktLine("\x02"+reason+"\n")+pktLine("\x01"+report)+"0000")
		return
	}
	io.WriteString(w, report)
}

func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// readPushRequest reads the ref update list at the head of a receive-pack
// body and stops at its flush packet. The rest is left unread in the returned
// reader. The second result is the refusal reason, or "".
func readPushRequest(body io.Reader) (*pushRequest, string) {
	br := bufio.NewReaderSize(body, 64<<10)
	req := &pushRequest{rest: br}
	var head bytes.Buffer
	for {
		payload, flush, err := readPkt(br, &head)
		switch {
		case errors.Is(err, errPktTooLarge):
			return nil, "push request is malformed"
		case err != nil:
			return nil, "push request is malformed"
		}
		if head.Len() > maxCommandBytes {
			return nil, "ref update list is too large"
		}
		if flush {
			break
		}
		line := string(payload)
		if len(req.cmds) == 0 && !strings.HasPrefix(line, "shallow ") {
			// Capabilities ride on the first command, after a NUL.
			if i := strings.IndexByte(line, 0); i >= 0 {
				req.caps = strings.Fields(line[i+1:])
				line = line[:i]
			}
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "shallow ") && len(req.cmds) == 0:
			continue
		case strings.HasPrefix(line, "push-cert"):
			return nil, "signed pushes are not supported"
		case strings.IndexByte(line, 0) >= 0:
			return nil, "push request is malformed"
		}
		cmd, ok := parsePushCommand(line)
		if !ok {
			return nil, "push request is malformed"
		}
		req.cmds = append(req.cmds, cmd)
	}
	if len(req.cmds) == 0 {
		// A bare flush is git's auth probe for large pushes. It updates
		// nothing, but nothing may follow it.
		if _, err := br.Peek(1); err != io.EOF {
			return nil, "push request has no ref updates"
		}
	}
	req.head = head.Bytes()
	return req, ""
}

var errPktTooLarge = errors.New("pkt-line too large")

// readPkt reads one pkt-line, appending its raw bytes to raw. flush is true
// for the 0000 packet.
func readPkt(br *bufio.Reader, raw *bytes.Buffer) (payload []byte, flush bool, err error) {
	var hdr [4]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, false, err
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil || strings.ContainsAny(string(hdr[:]), "+-") {
		return nil, false, errors.New("bad pkt-line length")
	}
	raw.Write(hdr[:])
	switch {
	case n == 0:
		return nil, true, nil
	case n < 4:
		return nil, false, errors.New("unsupported pkt-line")
	case n-4 > maxPktPayload:
		return nil, false, errPktTooLarge
	}
	payload = make([]byte, n-4)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, false, err
	}
	raw.Write(payload)
	return payload, false, nil
}

func parsePushCommand(line string) (pushCommand, bool) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 || parts[2] == "" {
		return pushCommand{}, false
	}
	old, nw := parts[0], parts[1]
	if len(old) != len(nw) || (len(old) != 40 && len(old) != 64) || !isHex(old) || !isHex(nw) {
		return pushCommand{}, false
	}
	return pushCommand{Old: old, New: nw, Ref: parts[2]}, true
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// validRefTail checks the part of a branch name after the namespace, using
// git's own rules for ref names, so what the gateway approves is what git
// would store.
func validRefTail(s string) bool {
	if strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.Contains(s, "//") ||
		strings.Contains(s, "..") || strings.Contains(s, "@{") || s == "@" {
		return false
	}
	for _, comp := range strings.Split(s, "/") {
		if !validRefComponent(comp) {
			return false
		}
	}
	return true
}

func validRefComponent(c string) bool {
	if c == "" || strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") || strings.Contains(c, "..") {
		return false
	}
	for _, r := range c {
		if r < 0x21 || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			return false
		}
	}
	return true
}
