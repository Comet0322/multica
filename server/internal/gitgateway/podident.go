package gitgateway

// Daemon reads. The stock Multica daemon clones and fetches repositories for
// its own checkouts and carries no task token, so the gateway identifies it
// by the source IP of the connection instead: the manager knows which pod owns
// that address and whose tasks the pod holds. Identity by address is weak by
// design, so it only ever grants reads (clone, fetch). Writes need a task
// token.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// PodResolver is the gateway's view of which tasks a pod holds. It returns one
// Identity per task currently leased to the pod at ip, or none if the address
// holds no lease. Any error means the manager could not answer.
type PodResolver interface {
	ResolvePod(ctx context.Context, ip string) ([]Identity, error)
}

// readOnlyAction reports whether the action may be taken on the strength of a
// source address alone. Anything not listed, including every future write
// action, needs a task token.
func readOnlyAction(action string) bool {
	return action == ActionInfoRefs || action == ActionUploadPack
}

// serveDaemonRead handles a request that carries no task token.
func (g *Gateway) serveDaemonRead(w http.ResponseWriter, r *http.Request) {
	rec := AuditRecord{Action: "git.request"}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		rec.Reason = "task token required and source address is unknown"
		g.deny(w, r, rec, http.StatusUnauthorized)
		return
	}
	src := ip.String()

	rt, why := parseRoute(r.Method, r.URL)
	if why != "" {
		rec.Reason = why
		g.deny(w, r, rec, http.StatusForbidden)
		return
	}
	rec.Action, rec.Project = rt.action, rt.project
	if !readOnlyAction(rt.action) {
		rec.Reason = "writes need a task token; source address only grants reads"
		g.deny(w, r, rec, http.StatusForbidden)
		return
	}

	ids, err := g.cfg.PodResolver.ResolvePod(r.Context(), src)
	if err != nil {
		g.log.Error("git gateway pod lookup failed", "error", err)
		rec.Reason = "pod lookup failed"
		g.deny(w, r, rec, http.StatusServiceUnavailable)
		return
	}
	if len(ids) == 0 {
		rec.Reason = "source address holds no lease"
		g.deny(w, r, rec, http.StatusForbidden)
		return
	}

	// Any initiator on the pod with access lets the read through; the read
	// is refused only when every one of them is refused.
	denyStatus, denyReason := http.StatusForbidden, ""
	for _, id := range ids {
		status, reason := g.authorizeRead(r.Context(), id.Initiator, rt.project)
		if status == 0 {
			rec.TaskID, rec.WorkspaceID, rec.Initiator = id.TaskID, id.WorkspaceID, id.Initiator
			rec.Decision, rec.Time = AuditAllow, g.now().UTC()
			rec.Reason = fmt.Sprintf("daemon read by leased pod %s; initiator has at least access level %d", src, g.cfg.MinReadLevel)
			g.audit.write(rec)
			g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, rt)))
			return
		}
		if status == http.StatusServiceUnavailable {
			denyStatus = status
		}
		if denyReason == "" {
			denyReason = reason
		}
	}
	rec.Reason = fmt.Sprintf("no initiator on pod %s may read: %s", src, denyReason)
	g.deny(w, r, rec, denyStatus)
}

// PodVerifyHandler serves the endpoint the gateway calls to resolve a pod
// address: POST {"ip": "..."} answers 200 with {"identities": [...]}, empty
// when the address holds no lease. Expose it to the gateway only.
func PodVerifyHandler(lookup func(ip string) []Identity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			IP string `json:"ip"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&body); err != nil || net.ParseIP(body.IP) == nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ids := lookup(net.ParseIP(body.IP).String())
		if ids == nil {
			ids = []Identity{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"identities": ids})
	})
}

// HTTPPodResolver asks a manager's PodVerifyHandler which tasks a pod holds.
type HTTPPodResolver struct {
	URL    string
	Client *http.Client
}

func (h HTTPPodResolver) ResolvePod(ctx context.Context, ip string) ([]Identity, error) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	payload, _ := json.Marshal(map[string]string{"ip": ip})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gitgateway: manager answered %d", resp.StatusCode)
	}
	var out struct {
		Identities []Identity `json:"identities"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitgateway: manager reply: %w", err)
	}
	for _, id := range out.Identities {
		if strings.TrimSpace(id.TaskID) == "" {
			return nil, errors.New("gitgateway: manager reply has an identity without a task")
		}
	}
	return out.Identities, nil
}
