package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issueposition"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// ext-workflow (fork): child issues of a workflow run.

// ExtCreateChildIssueTx inserts a workflow child issue inside the engine's
// transaction. It shares Create's numbering and positioning, but not its own
// transaction, duplicate guard or assignment enqueue: the engine alone decides
// when a child's agent runs, and the run and its children commit together.
// Callers publish with ExtPublishIssueCreated after their commit.
func (s *IssueService) ExtCreateChildIssueTx(ctx context.Context, tx pgx.Tx, p IssueCreateParams) (db.Issue, error) {
	q := s.Queries.WithTx(tx)
	number, err := AllocateIssueNumber(ctx, q, p.WorkspaceID, ResolveIssueCountPolicy(ctx, s.Entitlements, p.WorkspaceID))
	if err != nil {
		return db.Issue{}, fmt.Errorf("allocate issue number: %w", err)
	}
	position, err := issueposition.NextTopPosition(ctx, tx, p.WorkspaceID, p.Status)
	if err != nil {
		return db.Issue{}, fmt.Errorf("next top position: %w", err)
	}
	issue, err := q.CreateIssue(ctx, db.CreateIssueParams{
		ID:            dbid.NewV7(),
		WorkspaceID:   p.WorkspaceID,
		Title:         p.Title,
		Description:   p.Description,
		Status:        p.Status,
		Priority:      p.Priority,
		AssigneeType:  p.AssigneeType,
		AssigneeID:    p.AssigneeID,
		CreatorType:   p.CreatorType,
		CreatorID:     p.CreatorID,
		ParentIssueID: p.ParentIssueID,
		Position:      position,
		Number:        number,
		ProjectID:     p.ProjectID,
		Properties:    []byte(`{}`),
	})
	if err != nil {
		return db.Issue{}, fmt.Errorf("create child issue: %w", err)
	}
	return issue, nil
}

// ExtPublishIssueCreated emits issue:created for a committed child issue, with
// the same issue map background creators (autopilot) send.
func (s *IssueService) ExtPublishIssueCreated(ctx context.Context, issue db.Issue, actorType, actorID string) {
	prefix := ""
	if ws, err := s.Queries.GetWorkspace(ctx, issue.WorkspaceID); err == nil {
		prefix = ws.IssuePrefix
	}
	s.publishIssueCreated(issue, nil, nil, actorType, actorID, IssueCreateOpts{
		BroadcastPayload: func(i db.Issue, _ []db.Attachment, _ []db.IssueLabel) map[string]any {
			return map[string]any{"issue": IssueToMapResolved(ctx, s.Queries, i, prefix)}
		},
	})
}
