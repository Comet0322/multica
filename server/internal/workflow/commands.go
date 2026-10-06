// server/internal/workflow/commands.go
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"
	"unicode"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type Command string

const (
	CommandAccept Command = "accept"
	CommandReject Command = "reject"
	CommandRetry  Command = "retry"
)

func firstToken(content string) string {
	t := strings.TrimLeft(content, " \t\r\n")
	if i := strings.IndexFunc(t, unicode.IsSpace); i >= 0 {
		return t[:i]
	}
	return t
}

// IsCommandToken reports whether token is one of the workflow commands
// (case-insensitive). The handler uses it to keep these comments from waking
// agents, the same way /note does.
func IsCommandToken(token string) bool {
	_, ok := commandFromToken(token)
	return ok
}

func commandFromToken(token string) (Command, bool) {
	switch strings.ToLower(token) {
	case "/accept":
		return CommandAccept, true
	case "/reject":
		return CommandReject, true
	case "/retry":
		return CommandRetry, true
	}
	return "", false
}

// ParseCommand recognises a command only as the comment's first token.
func ParseCommand(content string) (Command, bool) {
	return commandFromToken(firstToken(content))
}

// CommentEvent is the subset of a comment the command handler needs.
type CommentEvent struct {
	ID, IssueID, AuthorType, AuthorID, Content string
}

// HandleComment applies a /accept, /reject or /retry comment. Comments that
// are not commands, or are not on a workflow issue, are ignored silently.
func (e *Engine) HandleComment(ctx context.Context, c CommentEvent) error {
	cmd, ok := ParseCommand(c.Content)
	if !ok {
		return nil
	}
	issueID, err := util.ParseUUID(c.IssueID)
	if err != nil {
		return nil
	}
	issue, err := e.Q.GetIssue(ctx, issueID)
	if err != nil {
		return nil
	}
	stepMeta, isStep := readStepMeta(issue)
	_, isDef := readDefMeta(issue)
	if !isStep && !isDef {
		return nil
	}

	def := issue
	if isStep {
		// Find the definition by the run id in the step metadata, the same key
		// the engine tracks steps by, so a step whose parent link is lost
		// still resolves.
		runID, perr := util.ParseUUID(stepMeta.Run)
		if perr != nil {
			return nil
		}
		def, err = e.Q.GetIssue(ctx, runID)
		if err != nil {
			return nil
		}
	}
	if !isCreator(def, c) {
		_, err := e.systemComment(ctx, issue, refusal(def, cmd))
		return err
	}
	commentUUID, _ := util.ParseUUID(c.ID)

	switch {
	case isStep && (cmd == CommandAccept || cmd == CommandReject):
		ev := EventAccept
		if cmd == CommandReject {
			ev = EventReject
		}
		applied, err := e.ApplyEvent(ctx, issue, ev, commentUUID)
		if err != nil {
			return err
		}
		if !applied {
			_, err = e.systemComment(ctx, issue, "This step is not waiting for review (phase: "+string(stepMeta.Phase)+").")
		}
		return err
	case isStep && cmd == CommandRetry:
		applied, err := e.ApplyEvent(ctx, issue, EventRetry, commentUUID)
		if !applied {
			if err == nil {
				_, err = e.systemComment(ctx, issue, "Only a failed step can be retried (phase: "+string(stepMeta.Phase)+").")
			}
			return err
		}
		// The claim succeeded even when a later write failed, so the step may
		// be running: always reopen the definition so the tick can track it.
		return errors.Join(err, e.reopenDefinition(ctx, def))
	case isDef && cmd == CommandRetry:
		steps, err := e.loadSteps(ctx, def)
		if err != nil {
			return err
		}
		var errs []error
		retried := 0
		for _, s := range steps {
			if s.meta.Phase != PhaseFailed {
				continue
			}
			applied, aerr := e.ApplyEvent(ctx, s.issue, EventRetry, commentUUID)
			if aerr != nil {
				errs = append(errs, aerr)
			}
			if applied {
				retried++
			}
		}
		if retried == 0 && len(errs) == 0 {
			reopened, rerr := e.reopenIfBlocked(ctx, def)
			if rerr != nil {
				return rerr
			}
			msg := "No failed steps to retry."
			if reopened {
				msg = "No failed steps to retry; re-evaluating the workflow."
			}
			_, err = e.systemComment(ctx, issue, msg)
			return err
		}
		if retried > 0 {
			errs = append(errs, e.reopenDefinition(ctx, def))
		}
		return errors.Join(errs...)
	default:
		_, err := e.systemComment(ctx, issue, "`/"+string(cmd)+"` applies to a step issue. Open the step and comment there.")
		return err
	}
}

// reopenIfBlocked reopens a blocked definition and reports whether it did.
func (e *Engine) reopenIfBlocked(ctx context.Context, def db.Issue) (bool, error) {
	if err := e.reopenDefinition(ctx, def); err != nil {
		return false, err
	}
	cur, err := e.Q.GetIssue(ctx, def.ID)
	if err != nil {
		return false, err
	}
	before, _ := readDefMeta(def)
	after, _ := readDefMeta(cur)
	return before.State == RunBlocked && after.State == RunRunning, nil
}

func isCreator(def db.Issue, c CommentEvent) bool {
	return c.AuthorType == "member" && def.CreatorType == "member" && util.UUIDToString(def.CreatorID) == c.AuthorID
}

// refusal explains why a command was not applied. It is always visible: a
// silent no-op would leave the commenter guessing.
func refusal(def db.Issue, cmd Command) string {
	if def.CreatorType != "member" {
		return "Workflow commands are unavailable for agent-created workflows. Recreate this workflow as a member to use `/" + string(cmd) + "`."
	}
	return "Only the workflow creator can use `/" + string(cmd) + "` here. This workflow was created by another member."
}

// RegisterListeners subscribes to comment:created and hands command comments
// to the engine off the publishing goroutine.
func RegisterListeners(bus *events.Bus, e *Engine) {
	bus.Subscribe(protocol.EventCommentCreated, func(ev events.Event) {
		c, ok := decodeCommentEvent(ev)
		if !ok {
			return
		}
		// Workflow commands come from members only; engine and agent text
		// must never trigger one.
		if c.AuthorType != "member" {
			return
		}
		if _, isCmd := ParseCommand(c.Content); !isCmd {
			return
		}
		go runCommand(e, c)
	})
}

// runCommand handles one command comment off the publishing goroutine. It
// recovers panics: the bus only protects its own synchronous call, so a panic
// here would otherwise take the whole server down.
func runCommand(e *Engine, c CommentEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("workflow command panicked", "issue_id", c.IssueID, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.HandleComment(ctx, c); err != nil {
		slog.Warn("workflow command failed", "issue_id", c.IssueID, "error", err)
	}
}

func decodeCommentEvent(ev events.Event) (CommentEvent, bool) {
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		return CommentEvent{}, false
	}
	raw, err := json.Marshal(payload["comment"])
	if err != nil {
		return CommentEvent{}, false
	}
	var wire struct {
		ID         string `json:"id"`
		IssueID    string `json:"issue_id"`
		AuthorType string `json:"author_type"`
		AuthorID   string `json:"author_id"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.IssueID == "" {
		return CommentEvent{}, false
	}
	return CommentEvent{ID: wire.ID, IssueID: wire.IssueID, AuthorType: wire.AuthorType, AuthorID: wire.AuthorID, Content: wire.Content}, true
}
