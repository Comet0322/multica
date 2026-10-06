// server/internal/workflow/expand.go
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const claimStaleAfter = 2 * time.Minute

// ExpandCandidates expands every eligible definition issue.
func (e *Engine) ExpandCandidates(ctx context.Context) error {
	cands, err := e.Q.ListWorkflowDefinitionCandidates(ctx, 50)
	if err != nil {
		return fmt.Errorf("list workflow candidates: %w", err)
	}
	var errs []error
	for _, def := range cands {
		if err := e.Expand(ctx, def); err != nil {
			slog.Warn("workflow expand failed", "issue_id", def.ID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func hashDescription(d pgtype.Text) string {
	sum := sha256.Sum256([]byte(d.String))
	return hex.EncodeToString(sum[:8])
}

// Expand claims def, validates its YAML, and creates one backlog step issue
// per node. It is safe to call concurrently and to re-run after a crash.
//
// The claim is fenced with a token (its claimed_at string) that is renewed
// before each write, including immediately before a step is stamped. An
// expander that stalls past claimStaleAfter and is reclaimed fails its next
// renewal and stops. The fence is best effort, not a lock: a stall longer than
// claimStaleAfter between the last renewal and the write it guards can still
// leave a stray unstamped child or, in the worst case, a duplicate stamped
// step. verifySteps rejects duplicates before the run starts.
func (e *Engine) Expand(ctx context.Context, def db.Issue) error {
	hash := hashDescription(def.Description)
	if dm, ok := readDefMeta(def); ok && dm.State == RunInvalid && dm.ErrorHash == hash {
		return nil // unchanged since it was last found invalid
	}
	token := e.now().UTC().Format(time.RFC3339Nano)
	claim, _ := json.Marshal(DefMeta{State: RunExpanding, ClaimedAt: token})
	claimed, err := e.Q.ClaimWorkflowDefinition(ctx, db.ClaimWorkflowDefinitionParams{
		Value: claim, ID: def.ID, WorkspaceID: def.WorkspaceID,
		StaleBefore: pgtype.Timestamptz{Time: e.now().Add(-claimStaleAfter), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // another instance owns it
	}
	if err != nil {
		return err
	}
	def = claimed
	// The description may have been edited between the snapshot and the claim;
	// validate and hash the claimed row so error_hash matches the errors posted.
	hash = hashDescription(def.Description)

	parsed, errs := Parse(def.Description.String)
	agents := map[string]db.Agent{}
	if len(errs) == 0 {
		list, err := e.Q.ListAgents(ctx, def.WorkspaceID)
		if err != nil {
			return err
		}
		for _, a := range list {
			agents[strings.ToLower(a.Name)] = a
		}
		for _, n := range parsed.Nodes {
			if _, ok := agents[strings.ToLower(n.Agent)]; !ok {
				errs = append(errs, fmt.Sprintf("node %q: unknown agent %q", n.ID, n.Agent))
			}
		}
	}
	if len(errs) > 0 {
		return e.markInvalid(ctx, def, token, hash, errs)
	}

	// Steps are found by the run id in metadata, never by parent_issue_id, so a
	// step that lost its parent link is still recognised and not duplicated.
	stamped, err := e.Q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidString(def.ID)})
	if err != nil {
		return err
	}
	byNode := map[string]db.Issue{}
	for _, k := range stamped {
		if m, ok := readStepMeta(k); ok {
			byNode[m.Node] = k
		}
	}
	// Orphans: children that were created but crashed before being stamped.
	existing, err := e.Q.ListWorkflowChildren(ctx, db.ListWorkflowChildrenParams{WorkspaceID: def.WorkspaceID, ParentIssueID: def.ID})
	if err != nil {
		return err
	}
	for _, n := range parsed.Nodes {
		if _, ok := byNode[n.ID]; ok {
			continue
		}
		agent := agents[strings.ToLower(n.Agent)]
		meta := StepMeta{
			Run: uuidString(def.ID), Node: n.ID, Deps: nonNil(n.DependsOn), Agent: agent.Name,
			AgentID: uuidString(agent.ID), Approval: n.Approval, MaxRetries: n.Retries(), Phase: PhasePending,
		}
		title := fmt.Sprintf("%s · %s", def.Title, n.ID)
		var orphan *db.Issue
		for i := range existing {
			if existing[i].Title == title {
				if _, has := readStepMeta(existing[i]); !has {
					orphan = &existing[i]
				}
			}
		}

		if e.beforeStep != nil {
			e.beforeStep(n.ID)
		}
		// Fence: renew the claim before every write; losing it ends this run.
		if token, err = e.renewClaim(ctx, def, token); err != nil || token == "" {
			return err
		}
		if orphan != nil {
			if err := e.writeMeta(ctx, *orphan, meta); err != nil {
				return err
			}
			continue
		}
		res, err := e.Issues.Create(ctx, service.IssueCreateParams{
			WorkspaceID:    def.WorkspaceID,
			Title:          title,
			Description:    pgtype.Text{String: n.Prompt, Valid: true},
			Status:         "backlog",
			Priority:       def.Priority,
			AssigneeType:   pgtype.Text{String: "agent", Valid: true},
			AssigneeID:     agent.ID,
			CreatorType:    def.CreatorType,
			CreatorID:      def.CreatorID,
			ParentIssueID:  def.ID,
			ProjectID:      def.ProjectID,
			AllowDuplicate: true,
		}, service.IssueCreateOpts{BroadcastPayload: e.Events.IssuePayload})
		if err != nil {
			return fmt.Errorf("create step %q: %w", n.ID, err)
		}
		// Renew again so a stall inside Create cannot stamp a node that another
		// expander has already claimed and created. A lost claim leaves this
		// child unstamped for the new owner to adopt or ignore.
		if token, err = e.renewClaim(ctx, def, token); err != nil || token == "" {
			return err
		}
		if err := e.writeMeta(ctx, res.Issue, meta); err != nil {
			return err
		}
	}

	// Exactly one stamped step per node must exist before the run starts. A
	// mismatch cannot heal by retrying, so the definition is blocked visibly.
	problem, err := e.verifySteps(ctx, def, parsed)
	if err != nil {
		return err
	}
	if problem != "" {
		return e.markBlocked(ctx, def, token, problem)
	}

	// Verify parentage before declaring the run started: every step must hang
	// off the definition issue. A mismatch does not stop the run (steps are
	// tracked by run id) but is surfaced so it is visible instead of silent.
	if err := e.warnOnMissingParents(ctx, def); err != nil {
		return err
	}

	if e.beforeStep != nil {
		e.beforeStep("finish")
	}
	if token, err = e.renewClaim(ctx, def, token); err != nil || token == "" {
		return err
	}
	prev, ok, err := e.finishFenced(ctx, def, token, DefMeta{State: RunRunning, Total: len(parsed.Nodes)})
	if err != nil || !ok {
		return err // ok=false: claim lost
	}
	// A crash between the running-meta write above and this status change leaves
	// state=running with status=todo; the engine tick reconciles that.
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: def.ID, WorkspaceID: def.WorkspaceID, Status: "in_progress"})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, prev, updated)
	return nil
}

// renewClaim moves the fencing token forward. It returns an empty token (and a
// nil error) when the claim was lost to another expander.
func (e *Engine) renewClaim(ctx context.Context, def db.Issue, token string) (string, error) {
	next := e.now().UTC().Format(time.RFC3339Nano)
	_, err := e.Q.RefreshWorkflowClaim(ctx, db.RefreshWorkflowClaimParams{
		NewClaimedAt: next, ID: def.ID, WorkspaceID: def.WorkspaceID, ExpectedClaimedAt: token,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return next, nil
}

// verifySteps checks that each node has exactly one stamped step issue and
// returns a description of the inconsistency, or "" when the steps are sound.
func (e *Engine) verifySteps(ctx context.Context, def db.Issue, parsed Definition) (string, error) {
	steps, err := e.Q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidString(def.ID)})
	if err != nil {
		return "", err
	}
	seen := map[string]int{}
	for _, s := range steps {
		if m, ok := readStepMeta(s); ok {
			seen[m.Node]++
		}
	}
	var problems []string
	for _, n := range parsed.Nodes {
		if seen[n.ID] != 1 {
			problems = append(problems, fmt.Sprintf("node %q has %d step issues (expected 1)", n.ID, seen[n.ID]))
		}
	}
	if len(steps) != len(parsed.Nodes) {
		problems = append(problems, fmt.Sprintf("found %d step issues for %d nodes", len(steps), len(parsed.Nodes)))
	}
	return strings.Join(problems, "; "), nil
}

// warnOnMissingParents comments on def when any step issue of the run is not
// its direct child.
func (e *Engine) warnOnMissingParents(ctx context.Context, def db.Issue) error {
	steps, err := e.Q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidString(def.ID)})
	if err != nil {
		return err
	}
	var bad []string
	for _, s := range steps {
		if s.ParentIssueID != def.ID {
			if m, ok := readStepMeta(s); ok {
				bad = append(bad, m.Node)
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	_, err = e.systemComment(ctx, def, "Warning: step issue(s) not linked to this issue as sub-issues: "+strings.Join(bad, ", ")+". The workflow still tracks them, but they will not show under this issue.")
	return err
}

// finishFenced writes terminal-for-now metadata only while the claim is still
// held. It reports false when the claim was lost.
func (e *Engine) finishFenced(ctx context.Context, def db.Issue, token string, m DefMeta) (db.Issue, bool, error) {
	raw, _ := json.Marshal(m)
	row, err := e.Q.FinishWorkflowExpansion(ctx, db.FinishWorkflowExpansionParams{
		Value: raw, ID: def.ID, WorkspaceID: def.WorkspaceID, ExpectedClaimedAt: token,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, false, nil
	}
	return row, err == nil, err
}

func (e *Engine) markInvalid(ctx context.Context, def db.Issue, token, hash string, errs []string) error {
	token, err := e.renewClaim(ctx, def, token)
	if err != nil || token == "" {
		return err
	}
	if _, ok, err := e.finishFenced(ctx, def, token, DefMeta{State: RunInvalid, ErrorHash: hash}); err != nil || !ok {
		return err
	}
	_, err = e.systemComment(ctx, def, "Workflow not started. Fix these problems, then edit the description:\n- "+strings.Join(errs, "\n- "))
	return err
}

// markBlocked takes an inconsistent definition out of the retry loop: it stores
// state=blocked, moves the issue to blocked and explains why in one comment.
func (e *Engine) markBlocked(ctx context.Context, def db.Issue, token, problem string) error {
	token, err := e.renewClaim(ctx, def, token)
	if err != nil || token == "" {
		return err
	}
	prev, ok, err := e.finishFenced(ctx, def, token, DefMeta{State: RunBlocked})
	if err != nil || !ok {
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: def.ID, WorkspaceID: def.WorkspaceID, Status: "blocked"})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, prev, updated)
	_, err = e.systemComment(ctx, def, "Workflow blocked: its step issues are inconsistent ("+problem+"). Delete the stray step issues or recreate the workflow.")
	return err
}

// systemComment inserts a system-authored comment and publishes it. It does
// not wake any agent.
func (e *Engine) systemComment(ctx context.Context, issue db.Issue, text string) (db.Comment, error) {
	row, err := e.Q.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
		Content: text, Type: "system",
	})
	if err != nil {
		return db.Comment{}, err
	}
	c := row.Comment()
	e.Events.CommentCreated(ctx, issue, c)
	return c, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
