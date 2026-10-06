package extworkflow

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DB-backed engine tests. Each test gets its own workspace; without a
// database they skip, like every other DB-backed package.

var (
	poolMu    sync.Mutex
	pool      *pgxpool.Pool
	envSerial atomic.Int64
)

func TestMain(m *testing.M) {
	code := m.Run()
	poolMu.Lock()
	if pool != nil {
		pool.Close()
	}
	poolMu.Unlock()
	os.Exit(code)
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolMu.Lock()
	defer poolMu.Unlock()
	if pool != nil {
		return pool
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	p, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Skipf("database not available: %v", err)
	}
	if err := p.Ping(context.Background()); err != nil {
		p.Close()
		t.Skipf("database not reachable: %v", err)
	}
	pool = p
	return pool
}

// recPublisher records Publisher calls.
type recPublisher struct {
	mu     sync.Mutex
	events []string // "<type>:<run_id>"
}

func (r *recPublisher) Publish(eventType, _, _, _ string, payload map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf("%s:%v", eventType, payload["run_id"]))
}

func (r *recPublisher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// fakeAccess allows every agent except the denied ones.
type fakeAccess struct {
	mu     sync.Mutex
	denied map[pgtype.UUID]bool
}

func (f *fakeAccess) CanInvokeAgent(_ context.Context, _ pgtype.UUID, _ string, _ pgtype.UUID, agentID pgtype.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.denied[agentID], nil
}

func (f *fakeAccess) deny(agentID pgtype.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.denied == nil {
		f.denied = map[pgtype.UUID]bool{}
	}
	f.denied[agentID] = true
}

type env struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	fx      *testutil.Fixture
	ws      pgtype.UUID
	user    pgtype.UUID
	runtime string
	bus     *events.Bus
	tasks   *service.TaskService
	issues  *service.IssueService
	access  *fakeAccess
	pub     *recPublisher
	engine  *Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	p := testPool(t)
	n := envSerial.Add(1)
	fx := testutil.New(p, "", "")
	user := fx.User(t, "Workflow User", fmt.Sprintf("extwf-%d-%d@multica.test", os.Getpid(), n))
	ws := fx.Workspace(t, "Workflow WS", fmt.Sprintf("extwf-%d-%d", os.Getpid(), n), testutil.Cols{"issue_prefix": "WF"})
	fx.WorkspaceID, fx.UserID = ws, user
	fx.Member(t, ws, user, "owner")
	rt := fx.Runtime(t, "wf-runtime")
	// Rows the engine creates. Registered after the workspace, so they are
	// removed before it (cleanup runs in reverse order).
	for _, stmt := range []string{
		`DELETE FROM issue WHERE workspace_id = $1`,
		`DELETE FROM agent_task_queue WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1)`,
		`DELETE FROM comment WHERE workspace_id = $1`,
		`DELETE FROM inbox_item WHERE workspace_id = $1`,
		`DELETE FROM activity_log WHERE workspace_id = $1`,
		`DELETE FROM issue_child_event WHERE workspace_id = $1`,
		`DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE workspace_id = $1)`,
		`DELETE FROM issue_wakeup WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_run_event WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_run_step WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_run WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_node WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow WHERE workspace_id = $1`,
	} {
		fx.Cleanup(t, stmt, ws)
	}
	q := db.New(p)
	bus := events.New()
	tasks := service.NewTaskService(q, p, nil, bus)
	issues := service.NewIssueService(q, p, bus, analytics.NoopClient{}, tasks)
	e := &env{
		pool: p, q: q, fx: fx, ws: util.MustParseUUID(ws), user: util.MustParseUUID(user), runtime: rt,
		bus: bus, tasks: tasks, issues: issues, access: &fakeAccess{}, pub: &recPublisher{},
	}
	e.engine = NewEngine(Deps{Pool: p, Queries: q, Issues: issues, Tasks: tasks, Access: e.access, Publisher: e.pub, Enabled: true})
	return e
}

func (e *env) agent(t *testing.T, name string) pgtype.UUID {
	t.Helper()
	return util.MustParseUUID(e.fx.Agent(t, name, e.runtime))
}

type wfNode struct {
	key, title  string
	agent       pgtype.UUID
	review      bool
	maxAttempts int
	deps        []string
}

// workflow inserts a workflow and its nodes in definition order.
func (e *env) workflow(t *testing.T, supervisor pgtype.UUID, maxRewinds int, nodes ...wfNode) pgtype.UUID {
	t.Helper()
	id := e.fx.Insert(t, "ext_workflow", testutil.Cols{
		"workspace_id": e.fx.WorkspaceID, "name": "Ship", "supervisor_agent_id": supervisor,
		"max_rewinds": maxRewinds, "creator_id": e.fx.UserID,
	})
	for i, n := range nodes {
		attempts := n.maxAttempts
		if attempts == 0 {
			attempts = 3
		}
		deps := n.deps
		if deps == nil {
			deps = []string{}
		}
		e.fx.Insert(t, "ext_workflow_node", testutil.Cols{
			"workflow_id": id, "workspace_id": e.fx.WorkspaceID, "key": n.key, "title": n.title, "agent_id": n.agent,
			"prompt": "Do " + n.title, "requires_review": n.review, "max_attempts": attempts, "depends_on": deps, "position": i,
		})
	}
	return util.MustParseUUID(id)
}

// parentIssue inserts an issue assigned to the workflow.
func (e *env) parentIssue(t *testing.T, workflowID pgtype.UUID, status string) pgtype.UUID {
	t.Helper()
	id := e.fx.Issue(t, "Ship feature", testutil.Cols{"status": status, "assignee_type": "workflow", "assignee_id": workflowID})
	// Fixture issues bypass the counter child issues draw their numbers from.
	e.fx.Exec(t, `UPDATE workspace SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) WHERE id = $1`, e.fx.WorkspaceID)
	return util.MustParseUUID(id)
}

func (e *env) start(t *testing.T, parent pgtype.UUID) db.ExtWorkflowRun {
	t.Helper()
	if err := e.engine.StartRun(context.Background(), parent, "member", e.user); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return e.run(t, parent)
}

// run returns the newest run of a parent issue, re-read.
func (e *env) run(t *testing.T, parent pgtype.UUID) db.ExtWorkflowRun {
	t.Helper()
	var id pgtype.UUID
	e.fx.QueryRow(t, `SELECT id FROM ext_workflow_run WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, parent).Scan(&id)
	run, err := e.q.GetExtWorkflowRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (e *env) step(t *testing.T, run db.ExtWorkflowRun, key string) db.ExtWorkflowRunStep {
	t.Helper()
	steps, err := e.q.ListExtWorkflowRunSteps(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.NodeKey == key {
			return s
		}
	}
	t.Fatalf("no step %q", key)
	return db.ExtWorkflowRunStep{}
}

func (e *env) issue(t *testing.T, id pgtype.UUID) db.Issue {
	t.Helper()
	issue, err := e.q.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

// latestTask is the newest task of a step for a role ("step"/"supervisor").
func (e *env) latestTask(t *testing.T, step db.ExtWorkflowRunStep, role string) db.AgentTaskQueue {
	t.Helper()
	task, err := e.q.GetLatestExtWorkflowTaskForStep(context.Background(), db.GetLatestExtWorkflowTaskForStepParams{
		StepID: step.ID, Role: pgtype.Text{String: role, Valid: true},
	})
	if err != nil {
		t.Fatalf("latest %s task of %s: %v", role, step.NodeKey, err)
	}
	return task
}

func (e *env) countTasks(t *testing.T, step db.ExtWorkflowRunStep, role string) int {
	t.Helper()
	return e.fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE ext_workflow_step_id = $1 AND ext_workflow_role = $2`, step.ID, role)
}

func (e *env) runEvents(t *testing.T, run db.ExtWorkflowRun) []string {
	t.Helper()
	rows, err := e.q.ListExtWorkflowRunEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(rows))
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
	}
	return kinds
}

func wantStepRow(t *testing.T, s db.ExtWorkflowRunStep, status StepStatus, attempts int) {
	t.Helper()
	if StepStatus(s.Status) != status || int(s.Attempts) != attempts {
		t.Fatalf("step %s = %s/%d, want %s/%d", s.NodeKey, s.Status, s.Attempts, status, attempts)
	}
}
