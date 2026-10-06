// server/internal/workflow/testenv_test.go
package workflow

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
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	seq      atomic.Int64
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		url := os.Getenv("DATABASE_URL")
		if url == "" {
			url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
		}
		p, err := pgxpool.New(context.Background(), url)
		if err == nil && p.Ping(context.Background()) == nil {
			pool = p
		}
	})
	if pool == nil {
		t.Skip("database not reachable")
	}
	return pool
}

// recorder is a fake TaskEnqueuer and EventPublisher.
type recorder struct {
	mu       sync.Mutex
	enqueued []string // "issue:<id>" or "mention:<id>:<comment>"
	failNext error
	updated  int
	comments []db.Comment
}

func (r *recorder) EnqueueTaskForIssue(_ context.Context, issue db.Issue, _ ...pgtype.UUID) (db.AgentTaskQueue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return db.AgentTaskQueue{}, err
	}
	r.enqueued = append(r.enqueued, "issue:"+util.UUIDToString(issue.ID))
	return db.AgentTaskQueue{}, nil
}

func (r *recorder) EnqueueTaskForMention(_ context.Context, issue db.Issue, _ pgtype.UUID, comment pgtype.UUID, _ service.RunOrigin) (db.AgentTaskQueue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueued = append(r.enqueued, "mention:"+util.UUIDToString(issue.ID)+":"+util.UUIDToString(comment))
	return db.AgentTaskQueue{}, nil
}

func (r *recorder) IssuePayload(db.Issue, []db.Attachment, []db.IssueLabel) map[string]any {
	return map[string]any{}
}
func (r *recorder) IssueUpdated(context.Context, db.Issue, db.Issue) {
	r.mu.Lock()
	r.updated++
	r.mu.Unlock()
}
func (r *recorder) CommentCreated(_ context.Context, _ db.Issue, c db.Comment) {
	r.mu.Lock()
	r.comments = append(r.comments, c)
	r.mu.Unlock()
}

type env struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	fx      *dbfx.Fixture
	ws      string
	user    string
	wsUUID  pgtype.UUID
	runtime string
	rec     *recorder
	engine  *Engine
	label   string // cached flow:test label id
}

func newEnv(t *testing.T) *env {
	t.Helper()
	p := testPool(t)
	n := seq.Add(1)
	fx := dbfx.New(p, "", "")
	user := fx.User(t, "Flow User", fmt.Sprintf("flow-%d-%d@example.test", os.Getpid(), n))
	ws := fx.Workspace(t, "Flow WS", fmt.Sprintf("flow-%d-%d", os.Getpid(), n))
	fx.WorkspaceID, fx.UserID = ws, user
	fx.Member(t, ws, user, "owner")
	rt := fx.Runtime(t, "flow-runtime")
	// Registered after the workspace fixture, so it runs before the workspace
	// is deleted and removes the issues the engine creates.
	fx.Cleanup(t, `DELETE FROM issue WHERE workspace_id = $1`, ws)
	q := db.New(p)
	rec := &recorder{}
	issues := service.NewIssueService(q, p, events.New(), analytics.NoopClient{}, nil)
	wsUUID, _ := util.ParseUUID(ws)
	return &env{pool: p, q: q, fx: fx, ws: ws, user: user, wsUUID: wsUUID, runtime: rt, rec: rec,
		engine: &Engine{Q: q, Issues: issues, Tasks: rec, Events: rec}}
}

func (e *env) agent(t *testing.T, name string) string {
	t.Helper()
	return e.fx.Agent(t, name, e.runtime)
}

// flowIssue creates a todo issue labelled flow:test, created by the member.
func (e *env) flowIssue(t *testing.T, title, desc string) db.Issue {
	t.Helper()
	id := e.fx.Issue(t, title, dbfx.Cols{"description": desc, "status": "todo"})
	if e.label == "" {
		e.label = e.fx.Insert(t, "issue_label", dbfx.Cols{"workspace_id": e.ws, "name": "flow:test", "resource_type": "issue", "color": "#888888"})
	}
	label := e.label
	e.fx.Exec(t, `INSERT INTO issue_to_label (issue_id, label_id) VALUES ($1, $2)`, id, label)
	e.fx.Cleanup(t, `DELETE FROM issue_to_label WHERE issue_id = $1`, id)
	uid, _ := util.ParseUUID(id)
	issue, err := e.q.GetIssue(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func dbfxCols(status string) dbfx.Cols { return dbfx.Cols{"status": status} }
