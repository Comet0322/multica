// server/internal/workflow/store.go
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// IssueCreator is *service.IssueService.
type IssueCreator interface {
	Create(ctx context.Context, p service.IssueCreateParams, opts service.IssueCreateOpts) (service.IssueCreateResult, error)
}

// TaskEnqueuer is *service.TaskService.
type TaskEnqueuer interface {
	EnqueueTaskForIssue(ctx context.Context, issue db.Issue, triggerCommentID ...pgtype.UUID) (db.AgentTaskQueue, error)
	EnqueueTaskForMention(ctx context.Context, issue db.Issue, agentID, triggerCommentID pgtype.UUID, origin service.RunOrigin) (db.AgentTaskQueue, error)
}

// EventPublisher emits events in the shapes existing listeners and the UI
// expect. handler.WorkflowEvents implements it.
type EventPublisher interface {
	IssuePayload(issue db.Issue, atts []db.Attachment, labels []db.IssueLabel) map[string]any
	IssueUpdated(ctx context.Context, prev, cur db.Issue)
	CommentCreated(ctx context.Context, issue db.Issue, c db.Comment)
}

// AgentInvokeChecker is the platform's agent invoke gate (handler.canInvokeAgent),
// judged for the workflow creator. A nil checker denies every agent, so a
// missing wiring can never let a workflow run an agent it should not.
type AgentInvokeChecker interface {
	CanInvokeAgent(ctx context.Context, agent db.Agent, creatorType, creatorID string) bool
}

type Engine struct {
	Q      *db.Queries
	Issues IssueCreator
	Tasks  TaskEnqueuer
	Events EventPublisher
	// Invoke gates which agents the workflow creator may run; nil denies all.
	Invoke AgentInvokeChecker
	// Now is overridable in tests; nil means time.Now.
	Now func() time.Time
	// beforeStep is a test-only hook called before each step write. It is nil in
	// production and exists only for deterministic race tests.
	beforeStep func(node string)
	// beforeClose is a test-only hook called between closeDefinition's status
	// write and its fenced state write. Nil in production; exists only for
	// deterministic race tests.
	beforeClose func()
	// beforeConvergeWrite is a test-only hook called between convergeStatus's
	// read and write. Nil in production; exists only for deterministic race tests.
	beforeConvergeWrite func()
	// beforeObserve is a test-only hook called before each step is observed; an
	// error fails that step's observation. Nil in production.
	beforeObserve func(node string) error
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// stepMetaToKeys flattens a step's metadata into wf_* primitive keys. Every
// key is always present so a merge fully replaces the previous step state.
func stepMetaToKeys(m StepMeta) map[string]any {
	return map[string]any{
		KeyRun:          m.Run,
		KeyNode:         m.Node,
		KeyDeps:         strings.Join(m.Deps, ","),
		KeyAgent:        m.Agent,
		KeyAgentID:      m.AgentID,
		KeyApproval:     m.Approval,
		KeyMaxRetries:   m.MaxRetries,
		KeyAttempts:     m.Attempts,
		KeyPhase:        string(m.Phase),
		KeyDispatchedAt: m.DispatchedAt,
	}
}

// defMetaToKeys flattens a definition's metadata into wf_* primitive keys.
// Every key is always present so a merge fully replaces the previous state.
func defMetaToKeys(m DefMeta) map[string]any {
	return map[string]any{
		KeyState:     string(m.State),
		KeyErrorHash: m.ErrorHash,
		KeyClaimedAt: m.ClaimedAt,
		KeyTotal:     m.Total,
	}
}

func stepMetaJSON(m StepMeta) ([]byte, error) { return json.Marshal(stepMetaToKeys(m)) }

func defMetaJSON(m DefMeta) ([]byte, error) { return json.Marshal(defMetaToKeys(m)) }

// metaBag decodes issue.metadata; nil when it is not a JSON object.
func metaBag(issue db.Issue) map[string]any {
	var bag map[string]any
	if err := json.Unmarshal(issue.Metadata, &bag); err != nil {
		return nil
	}
	return bag
}

func bagString(bag map[string]any, key string) string {
	s, _ := bag[key].(string)
	return s
}

func bagInt(bag map[string]any, key string) int {
	f, _ := bag[key].(float64)
	return int(f)
}

func bagBool(bag map[string]any, key string) bool {
	b, _ := bag[key].(bool)
	return b
}

// readStepMeta reads the wf_* step keys. Missing keys read as zero values; a
// step requires a non-empty wf_run.
func readStepMeta(i db.Issue) (StepMeta, bool) {
	bag := metaBag(i)
	run := bagString(bag, KeyRun)
	if run == "" {
		return StepMeta{}, false
	}
	var deps []string
	if d := bagString(bag, KeyDeps); d != "" {
		deps = strings.Split(d, ",")
	}
	return StepMeta{
		Run:          run,
		Node:         bagString(bag, KeyNode),
		Deps:         deps,
		Agent:        bagString(bag, KeyAgent),
		AgentID:      bagString(bag, KeyAgentID),
		Approval:     bagBool(bag, KeyApproval),
		MaxRetries:   bagInt(bag, KeyMaxRetries),
		Attempts:     bagInt(bag, KeyAttempts),
		Phase:        Phase(bagString(bag, KeyPhase)),
		DispatchedAt: bagString(bag, KeyDispatchedAt),
	}, true
}

// readDefMeta reads the wf_* definition keys. Missing keys read as zero
// values; a definition requires a non-empty wf_state.
func readDefMeta(i db.Issue) (DefMeta, bool) {
	bag := metaBag(i)
	state := bagString(bag, KeyState)
	if state == "" {
		return DefMeta{}, false
	}
	return DefMeta{
		State:     RunState(state),
		ErrorHash: bagString(bag, KeyErrorHash),
		ClaimedAt: bagString(bag, KeyClaimedAt),
		Total:     bagInt(bag, KeyTotal),
	}, true
}

// writeMeta merges every wf_* key of a StepMeta or DefMeta into the issue's
// metadata in one statement. MergeWorkflowMetadata returns no rows when the
// values are unchanged, which is not an error here.
func (e *Engine) writeMeta(ctx context.Context, issue db.Issue, v any) error {
	var (
		raw []byte
		err error
	)
	switch m := v.(type) {
	case StepMeta:
		raw, err = stepMetaJSON(m)
	case DefMeta:
		raw, err = defMetaJSON(m)
	default:
		return fmt.Errorf("workflow: writeMeta: unsupported type %T", v)
	}
	if err != nil {
		return err
	}
	_, err = e.Q.MergeWorkflowMetadata(ctx, db.MergeWorkflowMetadataParams{
		Value: raw, ID: issue.ID, WorkspaceID: issue.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func uuidString(u pgtype.UUID) string { return util.UUIDToString(u) }

// canInvoke reports whether the workflow creator may run agent.
func (e *Engine) canInvoke(ctx context.Context, agent db.Agent, creatorType string, creatorID pgtype.UUID) bool {
	if e.Invoke == nil {
		return false
	}
	return e.Invoke.CanInvokeAgent(ctx, agent, creatorType, uuidString(creatorID))
}
