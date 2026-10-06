// server/internal/workflow/store.go
package workflow

import (
	"context"
	"encoding/json"
	"errors"
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

type Engine struct {
	Q      *db.Queries
	Issues IssueCreator
	Tasks  TaskEnqueuer
	Events EventPublisher
	// Now is overridable in tests; nil means time.Now.
	Now func() time.Time
	// beforeStep is a test hook called before each step write.
	beforeStep func(node string)
	// beforeClose is a test hook called between closeDefinition's status write and its fenced state write.
	beforeClose func()
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func metaOf(issue db.Issue, v any) bool {
	var bag map[string]json.RawMessage
	if err := json.Unmarshal(issue.Metadata, &bag); err != nil {
		return false
	}
	raw, ok := bag[MetaKey]
	if !ok {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

func readStepMeta(i db.Issue) (StepMeta, bool) {
	var m StepMeta
	if !metaOf(i, &m) || m.Run == "" {
		return StepMeta{}, false
	}
	return m, true
}

func readDefMeta(i db.Issue) (DefMeta, bool) {
	var m DefMeta
	if !metaOf(i, &m) || m.State == "" {
		return DefMeta{}, false
	}
	return m, true
}

// writeMeta stores v under MetaKey. SetIssueMetadataKey returns no rows when
// the value is unchanged, which is not an error here.
func (e *Engine) writeMeta(ctx context.Context, issue db.Issue, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = e.Q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{
		Key: MetaKey, Value: raw, ID: issue.ID, WorkspaceID: issue.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func uuidString(u pgtype.UUID) string { return util.UUIDToString(u) }
