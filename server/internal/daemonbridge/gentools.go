package daemonbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
)

// SchemaCommand is one runnable CLI command as printed by
// `multica tools-schema`.
type SchemaCommand struct {
	Path  []string     `json:"path"`
	Short string       `json:"short"`
	Long  string       `json:"long"`
	Args  []SchemaArg  `json:"args"`
	Flags []SchemaFlag `json:"flags"`
}

// SchemaArg is a positional argument.
type SchemaArg struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Variadic bool   `json:"variadic"`
}

// SchemaFlag is a command flag.
type SchemaFlag struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Usage   string `json:"usage"`
	Default string `json:"default"`
}

// LoadSchema runs the CLI's tools-schema command.
func LoadSchema(ctx context.Context, binary string) ([]SchemaCommand, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "tools-schema").Output()
	if err != nil {
		return nil, fmt.Errorf("%s tools-schema: %w", binary, err)
	}
	var s struct {
		Commands []SchemaCommand `json:"commands"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("decode tools-schema: %w", err)
	}
	if len(s.Commands) == 0 {
		return nil, errors.New("tools-schema lists no commands")
	}
	return s.Commands, nil
}

// cliCall is one invocation of the multica CLI.
type cliCall struct {
	Args  []string
	Stdin string
}

// toolDef is a typed tool backed by one CLI command. The CLI stays the single
// implementation of every Multica action, so behaviour matches a local agent,
// while the run sees a schema instead of a command line.
//
// Naming rule, stated to the run in its brief: the tool for `multica a b-c`
// is `a_b_c`; positional arguments and flags keep the command's names with
// dashes turned into underscores.
type toolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
	// Write marks tools with side effects; identical calls within a task run once.
	Write bool
	Path  []string
	Build func(task daemon.Task, args map[string]any) (cliCall, error)
}

// deniedTop are commands that manage the local install or its credentials.
var deniedTop = map[string]bool{
	"daemon": true, "auth": true, "login": true, "setup": true, "config": true,
	"update": true, "runtime": true, "version": true, "completion": true, "tools-schema": true,
}

// deniedPaths are commands that make no sense with the work running elsewhere:
// they act on files of the bridge host, which the run cannot see or use.
var deniedPaths = map[string]bool{
	"repo checkout": true, "attachment upload": true, "attachment download": true,
}

var readVerbs = map[string]bool{
	"get": true, "list": true, "search": true, "history": true, "thread": true, "children": true,
	"runs": true, "timeline": true, "usage": true, "pull-requests": true, "run-messages": true,
	"show": true, "view": true, "tail": true, "status-list": true,
}

// kindAllow lists, per task kind, the command prefixes offered as tools. A
// run sees tens of tools, not the whole CLI, because a long tool list makes
// models pick worse and costs every turn.
var kindAllow = func() map[Kind][]string {
	read := []string{
		"issue get", "issue list", "issue search", "issue comment list", "issue children", "issue runs",
		"issue timeline", "issue run-messages", "issue pull-requests",
		"workspace member list", "workspace get", "agent list", "agent get", "squad list", "squad get",
		"project list", "project get", "label list", "property list", "skill list", "skill get",
		"chat history", "chat thread", "repo list",
	}
	work := append(append([]string{}, read...),
		"issue comment add", "issue comment update", "issue comment resolve", "issue comment unresolve",
		"issue status", "issue update", "issue create", "issue assign", "issue label", "issue property",
		"issue metadata", "issue subscriber", "issue wakeup", "issue rerun", "issue cancel-task",
		"issue reorder", "issue usage", "project", "label create", "autopilot get", "autopilot list",
	)
	return map[Kind][]string{
		KindIssue:     work,
		KindWakeup:    work,
		KindAutopilot: work,
		KindChat:      append(append([]string{}, work...), "chat"),
		KindSquad:     append(append([]string{}, work...), "squad activity", "squad member"),
		KindQuick: append(append([]string{}, read...),
			"issue create", "issue assign", "issue label", "issue property", "issue metadata"),
	}
}()

func (d toolDef) offeredFor(k Kind) bool {
	cmd := strings.Join(d.Path, " ")
	for _, p := range kindAllow[k] {
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return true
		}
	}
	return false
}

func toolName(path []string) string {
	return strings.ReplaceAll(strings.Join(path, "_"), "-", "_")
}

func ident(s string) string {
	if i := strings.IndexByte(s, '|'); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// skipFlag reports flags the typed tool does not expose: output formatting is
// fixed to JSON, and file-path flags refer to the bridge host's disk.
func skipFlag(name string) bool {
	return name == "output" || name == "allow-external-file" || name == "attachment" ||
		strings.HasSuffix(name, "-file") || strings.HasSuffix(name, "-stdin")
}

// generate builds the tools for every allowed command.
func generate(cmds []SchemaCommand) map[string]toolDef {
	defs := map[string]toolDef{}
	for _, c := range cmds {
		if len(c.Path) == 0 || deniedTop[c.Path[0]] || deniedPaths[strings.Join(c.Path, " ")] {
			continue
		}
		d := buildDef(c)
		defs[d.Name] = d
	}
	return defs
}

func buildDef(c SchemaCommand) toolDef {
	props := map[string]any{}
	var required []string
	for _, a := range c.Args {
		n := ident(a.Name)
		if a.Variadic {
			props[n] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		} else {
			props[n] = map[string]any{"type": "string"}
		}
		if a.Required {
			required = append(required, n)
		}
	}
	stdinOf := map[string]bool{}
	for _, f := range c.Flags {
		if strings.HasSuffix(f.Name, "-stdin") {
			stdinOf[strings.TrimSuffix(f.Name, "-stdin")] = true
		}
	}
	for _, f := range c.Flags {
		if skipFlag(f.Name) {
			continue
		}
		n := ident(f.Name)
		if _, taken := props[n]; taken {
			n += "_flag"
		}
		prop := flagSchema(f)
		prop["description"] = clip(f.Usage, 300)
		props[n] = prop
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	rawSchema, _ := json.Marshal(schema)

	desc := c.Short
	if first, _, _ := strings.Cut(strings.TrimSpace(c.Long), "\n\n"); first != "" && first != c.Short {
		desc += "\n" + clip(first, 500)
	}
	desc += "\n(CLI equivalent: multica " + strings.Join(c.Path, " ") + ")"

	d := toolDef{Name: toolName(c.Path), Description: desc, Schema: rawSchema, Path: c.Path,
		Write: !readVerbs[c.Path[len(c.Path)-1]]}
	d.Build = func(task daemon.Task, args map[string]any) (cliCall, error) {
		return buildCall(c, stdinOf, task, args)
	}
	return d
}

func flagSchema(f SchemaFlag) map[string]any {
	switch f.Type {
	case "bool":
		return map[string]any{"type": "boolean"}
	case "int", "int32", "int64":
		return map[string]any{"type": "integer"}
	case "float64", "float32":
		return map[string]any{"type": "number"}
	case "stringSlice", "stringArray":
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	default:
		return map[string]any{"type": "string"}
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func buildCall(c SchemaCommand, stdinOf map[string]bool, task daemon.Task, args map[string]any) (cliCall, error) {
	known := map[string]bool{}
	call := cliCall{Args: append([]string{}, c.Path...)}

	for i, a := range c.Args {
		n := ident(a.Name)
		known[n] = true
		v, present := args[n]
		if !present || v == nil || v == "" {
			if i == 0 && (c.Path[0] == "issue" || c.Path[0] == "squad") && task.IssueID != "" && (n == "id" || n == "issue_id") {
				v, present = task.IssueID, true
			}
		}
		if !present || v == nil || v == "" {
			if a.Required {
				return cliCall{}, fmt.Errorf("%s is required", n)
			}
			continue
		}
		vals, err := strList(v)
		if err != nil {
			return cliCall{}, fmt.Errorf("%s: %w", n, err)
		}
		call.Args = append(call.Args, vals...)
	}

	for _, f := range c.Flags {
		if skipFlag(f.Name) {
			continue
		}
		n := ident(f.Name)
		if _, dup := known[n]; dup {
			n += "_flag"
		}
		known[n] = true
		v, present := args[n]
		if !present || v == nil {
			continue
		}
		flag := "--" + f.Name
		switch f.Type {
		case "bool":
			if b, _ := v.(bool); b {
				call.Args = append(call.Args, flag)
			}
		case "stringSlice", "stringArray":
			vals, err := strList(v)
			if err != nil {
				return cliCall{}, fmt.Errorf("%s: %w", n, err)
			}
			for _, x := range vals {
				call.Args = append(call.Args, flag, x)
			}
		default:
			s := scalar(v)
			if s == "" {
				continue
			}
			if stdinOf[f.Name] && call.Stdin == "" {
				// Passing text on stdin keeps it verbatim; --flag value would
				// decode backslash escapes in it.
				call.Args = append(call.Args, flag+"-stdin")
				call.Stdin = s
				continue
			}
			call.Args = append(call.Args, flag, s)
		}
	}

	var unknown []string
	for k := range args {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return cliCall{}, fmt.Errorf("unknown argument(s): %s", strings.Join(unknown, ", "))
	}

	for _, f := range c.Flags {
		if f.Name == "output" {
			call.Args = append(call.Args, "--output", "json")
		}
	}
	// A comment-triggered run must reply in the trigger's thread.
	if strings.Join(c.Path, " ") == "issue comment add" && task.TriggerCommentID != "" && !hasFlag(call.Args, "--parent") &&
		len(call.Args) > 3 && call.Args[3] == task.IssueID {
		call.Args = append(call.Args, "--parent", task.TriggerCommentID)
	}
	return call, nil
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func strList(v any) ([]string, error) {
	switch x := v.(type) {
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, scalar(e))
		}
		return out, nil
	default:
		return []string{scalar(v)}, nil
	}
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	return m, nil
}
