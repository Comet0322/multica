package gitgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The MCP front door. It speaks the streamable HTTP transport in its simplest
// form: every POST carries one JSON-RPC message and is answered with one JSON
// body. It shares the git entry's task tokens and authorization, so a person
// can do through a tool only what their own GitLab access allows.

const (
	mcpMaxBody      = 1 << 20
	mcpMaxNoteSize  = 64 << 10
	mcpMaxTitleSize = 255
	mcpDefaultVer   = "2025-03-26"
)

var mcpVersions = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

// MCPHandler serves the gateway's MCP tools. Mount it at the URL injected
// into the task's mcp_config.
func (g *Gateway) MCPHandler() http.Handler {
	return http.HandlerFunc(g.serveMCP)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (g *Gateway) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := bearerOrBasicPassword(r)
	if token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="git gateway"`)
		http.Error(w, "task token required", http.StatusUnauthorized)
		return
	}
	id, err := g.cfg.Resolver.Resolve(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnknownToken):
		g.audit.write(AuditRecord{Action: "mcp.request", Decision: AuditDeny, Reason: "unknown or revoked task token", Time: g.now().UTC()})
		w.Header().Set("WWW-Authenticate", `Bearer realm="git gateway"`)
		http.Error(w, "unknown or revoked task token", http.StatusUnauthorized)
		return
	case err != nil:
		g.log.Error("mcp gateway token verification failed", "error", err)
		g.audit.write(AuditRecord{Action: "mcp.request", Decision: AuditDeny, Reason: "task token verification failed", Time: g.now().UTC()})
		http.Error(w, "task token verification failed", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{Code: -32600, Message: "invalid request"}})
		return
	}
	if len(req.ID) == 0 { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := rpcResponse{ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		ver := mcpDefaultVer
		if mcpVersions[p.ProtocolVersion] {
			ver = p.ProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "git-gateway", "version": "1"},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools}
	case "tools/call":
		resp.Result, resp.Error = g.callTool(r.Context(), id, req.Params)
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

type toolArgs struct {
	Project string `json:"project"`
	Search  string `json:"search"`
	State   string `json:"state"`
	IID     int    `json:"iid"`
	Body    string `json:"body"`

	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
}

// callTool runs one tool for the task's initiator. A refused or failed call is
// a tool result with isError set, so the model reads why; only a malformed
// call is a protocol error.
func (g *Gateway) callTool(ctx context.Context, id Identity, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	known := false
	for _, t := range mcpTools {
		known = known || t.Name == p.Name
	}
	if !known {
		return nil, &rpcError{Code: -32602, Message: "unknown tool " + p.Name}
	}
	var a toolArgs
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &a); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid arguments"}
		}
	}

	rec := AuditRecord{Action: "mcp." + p.Name, TaskID: id.TaskID, WorkspaceID: id.WorkspaceID, Initiator: id.Initiator}
	refuse := func(reason string) (any, *rpcError) {
		rec.Decision, rec.Reason, rec.Time = AuditDeny, reason, g.now().UTC()
		g.audit.write(rec)
		return toolResult("gateway: "+reason, true), nil
	}

	project, why := mcpProject(a.Project)
	if why != "" {
		return refuse(why)
	}
	rec.Project = project
	if why := a.validate(p.Name); why != "" {
		return refuse(why)
	}
	allowReason := fmt.Sprintf("initiator has at least access level %d", g.cfg.MinReadLevel)
	var mr mrRequest
	if p.Name == toolCreateMR {
		// Opening a merge request is a write: asked afresh, at the push level.
		if status, reason := g.authorizePush(ctx, id.Initiator, project); status != 0 {
			return refuse(reason)
		}
		var why string
		if mr, why = g.checkCreateMR(ctx, id, project, a); why != "" {
			return refuse(why)
		}
		allowReason = fmt.Sprintf("initiator has at least access level %d; source branch %s was pushed by this task and target %s is allowed",
			g.cfg.MinPushLevel, mr.source, mr.target)
	} else if status, reason := g.authorizeRead(ctx, id.Initiator, project); status != 0 {
		return refuse(reason)
	}
	rec.Decision, rec.Reason, rec.Time = AuditAllow, allowReason, g.now().UTC()
	g.audit.write(rec)

	var (
		out any
		err error
	)
	switch p.Name {
	case "list_branches":
		out, err = g.gitlab.listBranches(ctx, project, a.Search)
	case "get_merge_request":
		out, err = g.gitlab.getMergeRequest(ctx, project, a.IID)
	case "list_merge_requests":
		out, err = g.gitlab.listMergeRequests(ctx, project, a.stateOrDefault())
	case toolCreateMR:
		out, err = g.gitlab.createMergeRequest(ctx, project, mr)
	case "comment_on_merge_request":
		// The note is written with the admin token, so it says whose task it is.
		text := fmt.Sprintf("_Posted by an agent on behalf of %s._\n\n%s", id.Initiator, a.Body)
		out, err = g.gitlab.addMergeRequestNote(ctx, project, a.IID, text)
	}
	switch {
	case errors.Is(err, errNotFound):
		return toolResult("not found: check the project, merge request and branch names", true), nil
	case err != nil:
		g.log.Error("mcp gateway gitlab call failed", "tool", p.Name, "error", err)
		return toolResult("gitlab is unavailable", true), nil
	}
	text, _ := json.Marshal(out)
	return toolResult(string(text), false), nil
}

func (a toolArgs) stateOrDefault() string {
	if a.State == "" {
		return "opened"
	}
	return a.State
}

// validate checks the arguments a tool needs before anything leaves the gateway.
func (a toolArgs) validate(tool string) string {
	switch tool {
	case "get_merge_request", "comment_on_merge_request":
		if a.IID <= 0 {
			return "iid must be a positive merge request number"
		}
	case "list_merge_requests":
		switch a.stateOrDefault() {
		case "opened", "closed", "merged", "all":
		default:
			return "state must be opened, closed, merged or all"
		}
	}
	if tool == toolCreateMR {
		switch {
		case strings.TrimSpace(a.SourceBranch) == "":
			return "source_branch is required"
		case strings.TrimSpace(a.Title) == "":
			return "title is required"
		case len(a.Title) > mcpMaxTitleSize:
			return "title is too long"
		case len(a.Description) > mcpMaxNoteSize:
			return "description is too long"
		}
	}
	if tool == "comment_on_merge_request" {
		if strings.TrimSpace(a.Body) == "" {
			return "body is required"
		}
		if len(a.Body) > mcpMaxNoteSize {
			return "body is too long"
		}
	}
	return ""
}

// mcpProject normalizes a project path the way the git entry does: lowercase,
// no ".git", every segment valid.
func mcpProject(s string) (string, string) {
	s = strings.Trim(strings.ToLower(strings.TrimSpace(s)), "/")
	s = strings.TrimSuffix(s, ".git")
	segs := strings.Split(s, "/")
	if len(segs) < 2 {
		return "", "project must be a full path such as group/name"
	}
	for _, seg := range segs {
		if !validSegment(seg) {
			return "", "invalid project path"
		}
	}
	return s, ""
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

func schema(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var (
	projectProp = map[string]any{"type": "string", "description": "Full project path, e.g. group/name"}
	iidProp     = map[string]any{"type": "integer", "description": "Merge request number within the project"}
	readOnly    = map[string]any{"readOnlyHint": true}
)

var mcpTools = []mcpTool{
	{
		Name:        "list_branches",
		Description: "List branches of a GitLab project, optionally filtered by a search string.",
		InputSchema: schema([]string{"project"}, map[string]any{
			"project": projectProp,
			"search":  map[string]any{"type": "string", "description": "Only branches whose name contains this"},
		}),
		Annotations: readOnly,
	},
	{
		Name:        "get_merge_request",
		Description: "Get one merge request of a GitLab project.",
		InputSchema: schema([]string{"project", "iid"}, map[string]any{"project": projectProp, "iid": iidProp}),
		Annotations: readOnly,
	},
	{
		Name:        "list_merge_requests",
		Description: "List merge requests of a GitLab project; state defaults to opened.",
		InputSchema: schema([]string{"project"}, map[string]any{
			"project": projectProp,
			"state":   map[string]any{"type": "string", "enum": []string{"opened", "closed", "merged", "all"}},
		}),
		Annotations: readOnly,
	},
	{
		Name: toolCreateMR,
		Description: "Open a merge request from a branch this task pushed. The target defaults to the project's default branch. " +
			"The gateway appends who the request is for and which task made it to the description.",
		InputSchema: schema([]string{"project", "source_branch", "title"}, map[string]any{
			"project":       projectProp,
			"source_branch": map[string]any{"type": "string", "description": "Branch this task pushed through the gateway"},
			"target_branch": map[string]any{"type": "string", "description": "Branch to merge into; default is the project's default branch"},
			"title":         map[string]any{"type": "string"},
			"description":   map[string]any{"type": "string", "description": "Merge request description (Markdown)"},
		}),
	},
	{
		Name:        "comment_on_merge_request",
		Description: "Add a comment to a merge request.",
		InputSchema: schema([]string{"project", "iid", "body"}, map[string]any{
			"project": projectProp, "iid": iidProp,
			"body": map[string]any{"type": "string", "description": "Comment text (Markdown)"},
		}),
	},
}
