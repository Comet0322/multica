package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Several daemon replicas sharing one runtime row all call ClaimTaskForRuntime
// concurrently. These tests pin the multi-claimer contract a pooled runtime
// relies on.

type concurrentClaimFixture struct {
	pool      *pgxpool.Pool
	runtimeID pgtype.UUID
	taskIDs   map[string]bool
}

func newConcurrentClaimFixture(t *testing.T, taskCount, maxConcurrent int) concurrentClaimFixture {
	t.Helper()

	pool := sharedTestPool(t)
	bootstrap := testutil.New(pool, "", "")
	suffix := time.Now().UnixNano()
	ownerID := bootstrap.User(t,
		fmt.Sprintf("claim-conc-%d", suffix),
		fmt.Sprintf("claim-conc-%d@example.com", suffix),
	)
	workspaceID := bootstrap.Workspace(t,
		fmt.Sprintf("claim-conc-%d", suffix),
		fmt.Sprintf("claim-conc-%d", suffix),
	)
	fx := testutil.New(pool, workspaceID, ownerID)
	fx.Member(t, workspaceID, ownerID, "owner")
	runtimeID := fx.Runtime(t, "pool-runtime", testutil.Cols{
		"visibility": "public",
		"owner_id":   ownerID,
	})
	agentID := fx.Agent(t, "pool-agent", runtimeID, testutil.Cols{
		"owner_id":             ownerID,
		"max_concurrent_tasks": maxConcurrent,
	})

	// One issue per task: tasks of the same (issue, agent) serialize by design,
	// which would hide a double-claim behind the serialization fence.
	taskIDs := make(map[string]bool, taskCount)
	for i := 0; i < taskCount; i++ {
		issueID := fx.Issue(t, fmt.Sprintf("claim concurrency %d", i))
		taskID := fx.Task(t, agentID, testutil.Cols{
			"runtime_id": runtimeID,
			"issue_id":   issueID,
			"status":     "queued",
		})
		taskIDs[taskID] = true
	}
	return concurrentClaimFixture{
		pool:      pool,
		runtimeID: util.MustParseUUID(runtimeID),
		taskIDs:   taskIDs,
	}
}

// claimConcurrently runs `claimers` goroutines that each claim until the
// runtime reports no task, and returns how many times each task id was handed out.
func claimConcurrently(t *testing.T, f concurrentClaimFixture, claimers int) map[string]int {
	t.Helper()
	ctx := context.Background()
	svc := NewTaskService(db.New(f.pool), f.pool, nil, events.New())

	var (
		mu      sync.Mutex
		claimed = map[string]int{}
		wg      sync.WaitGroup
		start   = make(chan struct{})
	)
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				task, err := svc.ClaimTaskForRuntime(ctx, f.runtimeID)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if task == nil {
					return
				}
				mu.Lock()
				claimed[util.UUIDToString(task.ID)]++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	return claimed
}

func TestClaimTaskForRuntimeConcurrentClaimersNeverDoubleClaim(t *testing.T) {
	const tasks, claimers = 50, 16
	f := newConcurrentClaimFixture(t, tasks, 1000)

	claimed := claimConcurrently(t, f, claimers)

	for id, n := range claimed {
		if n != 1 {
			t.Errorf("task %s handed out %d times, want exactly 1", id, n)
		}
		if !f.taskIDs[id] {
			t.Errorf("claimed unknown task %s", id)
		}
	}
	if len(claimed) != tasks {
		t.Errorf("distinct tasks claimed = %d, want %d", len(claimed), tasks)
	}

	var queued int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue WHERE runtime_id = $1 AND status = 'queued'`,
		f.runtimeID,
	).Scan(&queued); err != nil {
		t.Fatalf("count queued: %v", err)
	}
	if queued != 0 {
		t.Errorf("%d tasks still queued after claimers drained the runtime", queued)
	}
}

func TestClaimTaskForRuntimeConcurrentClaimersRespectAgentCapacity(t *testing.T) {
	const tasks, claimers, maxConcurrent = 50, 16, 5
	f := newConcurrentClaimFixture(t, tasks, maxConcurrent)

	claimed := claimConcurrently(t, f, claimers)

	if len(claimed) > maxConcurrent {
		t.Errorf("dispatched %d tasks for an agent with max_concurrent_tasks=%d", len(claimed), maxConcurrent)
	}
}
