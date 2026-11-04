// Package multicatools gives the model the Multica operations an agent needs
// (read and update its issue, create sub-issues, comment), as tools that run on
// the host.
//
// They exist because a sandbox with no network cannot run the multica CLI. They
// are not a port of it: each tool is one narrow operation, and every one is
// confined to a Scope, so the task token, which the sandbox never sees, can
// only be used for what this task is for.
package multicatools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/tools"
)

// Client calls the Multica server as one task.
type Client struct {
	BaseURL     string
	Token       string
	WorkspaceID string
	AgentID     string
	TaskID      string
	HTTP        *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Workspace-ID", c.WorkspaceID)
	req.Header.Set("X-Agent-ID", c.AgentID)
	req.Header.Set("X-Task-ID", c.TaskID)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// HTTPError is a non-2xx answer from the server.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("server answered %d: %s", e.Status, e.Body) }

// Scope is the set of issues a task may touch: its own, and the ones it creates
// under it. A tool refuses any other id, so a confused or hostile model cannot
// use the task token on the rest of the workspace.
type Scope struct {
	mu     sync.Mutex
	issues map[string]struct{}
	root   string
}

// NewScope returns a scope holding only the task's own issue.
func NewScope(taskIssueID string) *Scope {
	return &Scope{issues: map[string]struct{}{taskIssueID: {}}, root: taskIssueID}
}

func (s *Scope) allows(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.issues[id]
	return ok
}

func (s *Scope) add(id string) {
	s.mu.Lock()
	s.issues[id] = struct{}{}
	s.mu.Unlock()
}

// Tools returns the Multica tools for one task.
func Tools(c *Client, scope *Scope) []tools.Tool {
	return []tools.Tool{
		issueGet{c, scope}, issueCreate{c, scope}, issueUpdate{c, scope},
		commentList{c, scope}, commentAdd{c, scope},
	}
}

type env struct {
	c     *Client
	scope *Scope
}

func (e env) check(id string) (tools.Result, bool) {
	if id == "" {
		return tools.Result{Output: "issue_id is required", IsError: true}, false
	}
	if !e.scope.allows(id) {
		return tools.Result{Output: fmt.Sprintf("issue %s is outside this task: you may only use your own issue (%s) and the sub-issues you created", id, e.scope.root), IsError: true}, false
	}
	return tools.Result{}, true
}

// answer turns a server failure the model can act on into a Result.
func answer(err error) (tools.Result, error) {
	var he *HTTPError
	if errors.As(err, &he) && he.Status >= 400 && he.Status < 500 {
		return tools.Result{Output: he.Error(), IsError: true}, nil
	}
	return tools.Result{}, err
}

func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func s(in map[string]any, k string) string { v, _ := in[k].(string); return v }

func pretty(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func issuePath(id string, parts ...string) string {
	p := "/api/issues/" + url.PathEscape(id)
	for _, x := range parts {
		p += "/" + x
	}
	return p
}

// issue_get

type issueGet env

func (issueGet) Spec() tools.Spec {
	return tools.Spec{Name: "issue_get", Description: "Read an issue: title, description, status, priority and so on. Defaults to your own issue.",
		Schema: obj(map[string]any{"issue_id": str("Issue id; omit for the issue you were assigned.")})}
}

func (t issueGet) Run(ctx context.Context, in map[string]any) (tools.Result, error) {
	id := s(in, "issue_id")
	if id == "" {
		id = t.scope.root
	}
	if r, ok := (env(t)).check(id); !ok {
		return r, nil
	}
	var out any
	if err := t.c.do(ctx, http.MethodGet, issuePath(id), nil, &out); err != nil {
		return answer(err)
	}
	return tools.Result{Output: pretty(out)}, nil
}

// issue_create

type issueCreate env

func (issueCreate) Spec() tools.Spec {
	return tools.Spec{Name: "issue_create", Description: "Create a sub-issue. It is a child of your own issue unless parent_id names another issue you created.",
		Schema: obj(map[string]any{
			"title":       str("Issue title."),
			"description": str("Issue description (markdown)."),
			"status":      str("Initial status, e.g. todo."),
			"priority":    str("urgent, high, medium, low or none."),
			"parent_id":   str("Parent issue; defaults to your own issue."),
		}, "title")}
}

func (t issueCreate) Run(ctx context.Context, in map[string]any) (tools.Result, error) {
	title := s(in, "title")
	if strings.TrimSpace(title) == "" {
		return tools.Result{Output: "title is required", IsError: true}, nil
	}
	parent := s(in, "parent_id")
	if parent == "" {
		parent = t.scope.root
	}
	if r, ok := (env(t)).check(parent); !ok {
		return r, nil
	}
	body := map[string]any{"title": title, "parent_issue_id": parent}
	for _, k := range []string{"description", "status", "priority"} {
		if v := s(in, k); v != "" {
			body[k] = v
		}
	}
	var out struct {
		ID string `json:"id"`
	}
	var raw json.RawMessage
	if err := t.c.do(ctx, http.MethodPost, "/api/issues", body, &raw); err != nil {
		return answer(err)
	}
	_ = json.Unmarshal(raw, &out)
	if out.ID == "" {
		return tools.Result{}, fmt.Errorf("issue_create: server returned no id: %s", raw)
	}
	t.scope.add(out.ID)
	return tools.Result{Output: pretty(json.RawMessage(raw))}, nil
}

// issue_update

type issueUpdate env

func (issueUpdate) Spec() tools.Spec {
	return tools.Spec{Name: "issue_update", Description: "Change an issue's title, description, status or priority. Only the fields you pass change.",
		Schema: obj(map[string]any{
			"issue_id":    str("Issue id; omit for your own issue."),
			"title":       str("New title."),
			"description": str("New description."),
			"status":      str("New status, e.g. in_progress, in_review, done."),
			"priority":    str("New priority."),
		})}
}

func (t issueUpdate) Run(ctx context.Context, in map[string]any) (tools.Result, error) {
	id := s(in, "issue_id")
	if id == "" {
		id = t.scope.root
	}
	if r, ok := (env(t)).check(id); !ok {
		return r, nil
	}
	body := map[string]any{}
	for _, k := range []string{"title", "description", "status", "priority"} {
		if v := s(in, k); v != "" {
			body[k] = v
		}
	}
	if len(body) == 0 {
		return tools.Result{Output: "nothing to change: pass title, description, status or priority", IsError: true}, nil
	}
	var out any
	if err := t.c.do(ctx, http.MethodPut, issuePath(id), body, &out); err != nil {
		return answer(err)
	}
	return tools.Result{Output: pretty(out)}, nil
}

// comment_list

type commentList env

func (commentList) Spec() tools.Spec {
	return tools.Spec{Name: "comment_list", Description: "List the comments on an issue. Defaults to your own issue.",
		Schema: obj(map[string]any{"issue_id": str("Issue id; omit for your own issue.")})}
}

func (t commentList) Run(ctx context.Context, in map[string]any) (tools.Result, error) {
	id := s(in, "issue_id")
	if id == "" {
		id = t.scope.root
	}
	if r, ok := (env(t)).check(id); !ok {
		return r, nil
	}
	var out any
	if err := t.c.do(ctx, http.MethodGet, issuePath(id, "comments"), nil, &out); err != nil {
		return answer(err)
	}
	return tools.Result{Output: pretty(out)}, nil
}

// comment_add

type commentAdd env

func (commentAdd) Spec() tools.Spec {
	return tools.Spec{Name: "comment_add", Description: "Post a comment on an issue as the agent. Defaults to your own issue.",
		Schema: obj(map[string]any{
			"content":   str("Comment text (markdown)."),
			"issue_id":  str("Issue id; omit for your own issue."),
			"parent_id": str("Comment id to reply to."),
		}, "content")}
}

func (t commentAdd) Run(ctx context.Context, in map[string]any) (tools.Result, error) {
	id := s(in, "issue_id")
	if id == "" {
		id = t.scope.root
	}
	if r, ok := (env(t)).check(id); !ok {
		return r, nil
	}
	content := s(in, "content")
	if strings.TrimSpace(content) == "" {
		return tools.Result{Output: "content is required", IsError: true}, nil
	}
	body := map[string]any{"content": content}
	if p := s(in, "parent_id"); p != "" {
		body["parent_id"] = p
	}
	var out any
	if err := t.c.do(ctx, http.MethodPost, issuePath(id, "comments"), body, &out); err != nil {
		return answer(err)
	}
	return tools.Result{Output: pretty(out)}, nil
}
