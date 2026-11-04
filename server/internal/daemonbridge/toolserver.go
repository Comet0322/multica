package daemonbridge

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/multica-ai/multica/server/internal/daemon"
)

// MCPEndpoint is how a backend reaches the tools of one task over MCP.
type MCPEndpoint struct {
	// URL is the Streamable HTTP endpoint.
	URL string `json:"url"`
	// Token is the bearer token for this task only. It is minted by the bridge,
	// is useless once the task ends, and is not a Multica, model or git credential.
	Auth string `json:"token"`
}

// BriefNoter is implemented by a ToolRunner that explains its tools to the run.
type BriefNoter interface {
	BriefNote(kind Kind) string
}

// TaskScoped is implemented by a ToolRunner that serves tools per task.
type TaskScoped interface {
	// Open makes the task's tools reachable and returns where, or nil when the
	// runner has no network endpoint.
	Open(task daemon.Task, kind Kind) *MCPEndpoint
	// Close revokes everything Open handed out for the task.
	Close(taskID string)
}

// ToolServer exposes the typed Multica tools over MCP and as a ToolRunner.
//
// The tools run on the bridge host with the task-scoped token; the engine
// calling them only holds a bearer for this server, so nothing it can read
// opens Multica, the model provider or git.
type ToolServer struct {
	cli *CLITool
	// BaseURL is the externally reachable URL of this server, used to build
	// MCPEndpoint URLs, for example "http://bridge.internal:19515".
	BaseURL string
	// Secret makes a task's bearer stable across bridge restarts, so an engine
	// session that outlives the process keeps working. Without it every Open
	// mints a fresh random token.
	Secret []byte

	defs map[string]toolDef

	mu     sync.Mutex
	byTok  map[string]*taskTools
	byTask map[string]*taskTools
}

type taskTools struct {
	task daemon.Task
	kind Kind
	auth string

	mu   sync.Mutex
	done map[string]ToolResult // write calls already made, by call fingerprint
}

// NewToolServer builds a server whose tools run through cli and are generated
// from the CLI's own command schema.
func NewToolServer(cli *CLITool, baseURL string, schema []SchemaCommand) *ToolServer {
	s := &ToolServer{
		cli: cli, BaseURL: strings.TrimRight(baseURL, "/"),
		defs: map[string]toolDef{}, byTok: map[string]*taskTools{}, byTask: map[string]*taskTools{},
	}
	s.defs = generate(schema)
	return s
}

// Open implements TaskScoped.
func (s *ToolServer) Open(task daemon.Task, kind Kind) *MCPEndpoint {
	tt := &taskTools{task: task, kind: kind, auth: s.mintToken(task.ID), done: map[string]ToolResult{}}
	if tt.auth == "" {
		return nil
	}
	s.mu.Lock()
	if old := s.byTask[task.ID]; old != nil {
		delete(s.byTok, old.auth)
	}
	s.byTok[tt.auth], s.byTask[task.ID] = tt, tt
	s.mu.Unlock()
	if s.BaseURL == "" {
		return nil
	}
	return &MCPEndpoint{URL: s.BaseURL + "/mcp", Auth: tt.auth}
}

func (s *ToolServer) mintToken(taskID string) string {
	if len(s.Secret) > 0 {
		m := hmac.New(sha256.New, s.Secret)
		m.Write([]byte(taskID))
		return "bt_" + hex.EncodeToString(m.Sum(nil)[:24])
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return "bt_" + hex.EncodeToString(buf)
}

// Close implements TaskScoped.
func (s *ToolServer) Close(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tt := s.byTask[taskID]; tt != nil {
		delete(s.byTok, tt.auth)
		delete(s.byTask, taskID)
	}
}

// Specs implements ToolRunner.
func (s *ToolServer) Specs(_ daemon.Task, kind Kind) []ToolSpec { return s.specs(kind) }

func (s *ToolServer) specs(kind Kind) []ToolSpec {
	names := make([]string, 0, len(s.defs))
	for n, d := range s.defs {
		if d.offeredFor(kind) {
			names = append(names, n)
		}
	}
	sort.Strings(names) // stable order keeps provider-side prompt caches warm
	out := make([]ToolSpec, 0, len(names)+1)
	for _, n := range names {
		d := s.defs[n]
		out = append(out, ToolSpec{Name: d.Name, Description: d.Description, InputSchema: d.Schema})
	}
	return out
}

// BriefNote is prepended to the run's system text. The rendered brief is the
// local daemon's and tells the agent to run `multica ...` commands; this tells
// it how those instructions map onto the tools it actually has.
func (s *ToolServer) BriefNote(kind Kind) string {
	var names []string
	for _, sp := range s.specs(kind) {
		names = append(names, sp.Name)
	}
	return "## Tool mode\n\n" +
		"The `multica` command line is NOT available in this runtime and you have no shell access to Multica. " +
		"Wherever the instructions below tell you to run a `multica ...` command, call the equivalent tool instead.\n\n" +
		"- The tool for `multica a b-c` is named `a_b_c`; e.g. `multica issue comment add` is `issue_comment_add`.\n" +
		"- Positional arguments and flags keep the command's names, with dashes written as underscores " +
		"(`--parent` is `parent`, `<issue-id>` is `issue_id`). A flag without a value is a boolean; a repeatable flag is an array.\n" +
		"- Results are JSON. Text flags such as `content` and `description` take the text as-is: no shell quoting, no `-stdin` or `-file` variants, no `--output`.\n" +
		"- `issue_id` may be left out for the issue this task is about.\n" +
		"- Git is not available in this runtime: `multica repo checkout` and any other command that needs a repository cannot be used.\n" +
		"- A command with no tool below is unavailable: do not try to work around it; say in your reply that you could not do that step.\n\n" +
		"Tools available for this task: " + strings.Join(names, ", ") + ".\n"
}

// Run implements ToolRunner for backends that deliver tool calls as events.
func (s *ToolServer) Run(ctx context.Context, task daemon.Task, req ToolRequest) ToolResult {
	s.mu.Lock()
	tt := s.byTask[task.ID]
	s.mu.Unlock()
	if tt == nil {
		tt = &taskTools{task: task, kind: classify(task), done: map[string]ToolResult{}}
	}
	return s.call(ctx, tt, req.Name, req.Args)
}

func (s *ToolServer) call(ctx context.Context, tt *taskTools, name string, raw json.RawMessage) ToolResult {
	def, ok := s.defs[name]
	if !ok || !def.offeredFor(tt.kind) {
		return errResult("unknown tool %q", name)
	}
	args, err := decodeArgs(raw)
	if err != nil {
		return errResult("%v", err)
	}
	call, err := def.Build(tt.task, args)
	if err != nil {
		return errResult("%v", err)
	}
	var fp string
	if def.Write {
		// A crashed turn is replayed by some engines, which repeats its tool
		// calls. The same write within a task must not land twice.
		sum := sha256.Sum256([]byte(name + "\x00" + strings.Join(call.Args, "\x00") + "\x00" + call.Stdin))
		fp = hex.EncodeToString(sum[:])
		tt.mu.Lock()
		prev, seen := tt.done[fp]
		tt.mu.Unlock()
		if seen {
			prev.Output += "\n[already done earlier in this task; not repeated]"
			return prev
		}
	}
	body, _ := json.Marshal(cliArgs{Args: call.Args, Stdin: call.Stdin})
	res := s.cli.Run(ctx, tt.task, ToolRequest{Name: CLIToolName, Args: body})
	if def.Write && !res.IsError {
		tt.mu.Lock()
		tt.done[fp] = res
		tt.mu.Unlock()
	}
	return res
}

// ---- MCP over Streamable HTTP (JSON responses) ----------------------------

var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const maxMCPBody = 1 << 20

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ServeHTTP serves POST /mcp. Authentication is the per-task bearer token.
func (s *ToolServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// No server-initiated messages; the spec lets a server decline the stream.
		http.Error(w, "no event stream", http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	tt := s.byTok[tok]
	s.mu.Unlock()
	if tok == "" || tt == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="bridge"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPBody+1))
	if err != nil || len(body) > maxMCPBody {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	var batch []rpcRequest
	if trimmed := strings.TrimSpace(string(body)); strings.HasPrefix(trimmed, "[") {
		if json.Unmarshal(body, &batch) != nil {
			writeRPC(w, rpcError(nil, -32700, "parse error"))
			return
		}
	} else {
		var one rpcRequest
		if json.Unmarshal(body, &one) != nil {
			writeRPC(w, rpcError(nil, -32700, "parse error"))
			return
		}
		batch = []rpcRequest{one}
	}
	var replies []any
	for _, req := range batch {
		if len(req.ID) == 0 { // notification
			continue
		}
		replies = append(replies, s.dispatch(r.Context(), tt, req))
	}
	switch len(replies) {
	case 0:
		w.WriteHeader(http.StatusAccepted)
	case 1:
		writeRPC(w, replies[0])
	default:
		writeRPC(w, replies)
	}
}

func (s *ToolServer) dispatch(ctx context.Context, tt *taskTools, req rpcRequest) any {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := mcpVersions[0]
		for _, known := range mcpVersions {
			if p.ProtocolVersion == known {
				v = known
			}
		}
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "multica-bridge", "version": "1"},
		})
	case "ping":
		return rpcResult(req.ID, map[string]any{})
	case "tools/list":
		tools := make([]map[string]any, 0)
		for _, sp := range s.specs(tt.kind) {
			tools = append(tools, map[string]any{"name": sp.Name, "description": sp.Description, "inputSchema": json.RawMessage(sp.InputSchema)})
		}
		return rpcResult(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil || p.Name == "" {
			return rpcError(req.ID, -32602, "invalid params")
		}
		res := s.call(ctx, tt, p.Name, p.Arguments)
		return rpcResult(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": res.Output}},
			"isError": res.IsError,
		})
	default:
		return rpcError(req.ID, -32601, fmt.Sprintf("method %q not found", req.Method))
	}
}

func rpcResult(id json.RawMessage, result any) any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id json.RawMessage, code int, msg string) any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func writeRPC(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
