package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/k8sruntime/relay"
)

type fakeServer struct {
	mu        sync.Mutex
	queue     []*daemon.Task
	claimed   []int // max_tasks of every claim call
	status    map[string]string
	statusErr error
	leases    []string
	fails     map[string]string // task id -> failure reason
	registers int
}

func (s *fakeServer) Register(_ context.Context, req map[string]any) (*daemon.RegisterResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registers++
	return &daemon.RegisterResponse{Runtimes: []daemon.Runtime{{ID: "rt-1", Provider: "claude"}}}, nil
}
func (s *fakeServer) SendHeartbeat(context.Context, string) (*daemon.HeartbeatResponse, error) {
	return &daemon.HeartbeatResponse{}, nil
}
func (s *fakeServer) ClaimTasks(_ context.Context, _ string, _ []string, max int) ([]*daemon.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimed = append(s.claimed, max)
	n := max
	if n > len(s.queue) {
		n = len(s.queue)
	}
	out := s.queue[:n]
	s.queue = s.queue[n:]
	return out, nil
}
func (s *fakeServer) ExtendTaskPrepareLease(_ context.Context, _, taskID string) error {
	s.mu.Lock()
	s.leases = append(s.leases, taskID)
	s.mu.Unlock()
	return nil
}
func (s *fakeServer) GetTaskStatus(_ context.Context, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusErr != nil {
		return "", s.statusErr
	}
	return s.status[id], nil
}
func (s *fakeServer) FailTask(_ context.Context, id, _, _, _, _, reason string, _ bool, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails == nil {
		s.fails = map[string]string{}
	}
	s.fails[id] = reason
	return nil
}

type fakeTokens struct {
	mu sync.Mutex
	m  map[string]relay.Task
}

func (f *fakeTokens) Register(tok string, t relay.Task) {
	f.mu.Lock()
	if f.m == nil {
		f.m = map[string]relay.Task{}
	}
	f.m[tok] = t
	f.mu.Unlock()
}
func (f *fakeTokens) Unregister(tok string) {
	f.mu.Lock()
	delete(f.m, tok)
	f.mu.Unlock()
}
func (f *fakeTokens) has(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.m[tok]
	return ok
}

type fakeDriver struct {
	mu        sync.Mutex
	pods      map[string]*PodInfo
	inputs    map[string][]byte
	createErr error
	deleted   []string
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{pods: map[string]*PodInfo{}, inputs: map[string][]byte{}}
}
func (d *fakeDriver) Create(_ context.Context, spec PodSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.createErr != nil {
		return d.createErr
	}
	d.pods[spec.Name] = &PodInfo{Name: spec.Name, Phase: PhasePending, Annotations: spec.Annotations}
	d.inputs[spec.Name] = spec.Input
	return nil
}
func (d *fakeDriver) List(context.Context) ([]PodInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []PodInfo
	for _, p := range d.pods {
		out = append(out, *p)
	}
	return out, nil
}
func (d *fakeDriver) Input(_ context.Context, name string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.inputs[name]
	if !ok {
		return nil, errors.New("no input")
	}
	return b, nil
}
func (d *fakeDriver) Delete(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pods, name)
	d.deleted = append(d.deleted, name)
	return nil
}
func (d *fakeDriver) setPhase(name string, ph Phase) {
	d.mu.Lock()
	d.pods[name].Phase = ph
	d.mu.Unlock()
}

type harness struct {
	c   *Controller
	srv *fakeServer
	tok *fakeTokens
	drv *fakeDriver
	now time.Time
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	cfg := Config{
		DaemonID: "d1", Workspaces: []string{"ws-1"}, Providers: []string{"claude"},
		MaxPods: 2, LeaseInterval: 15 * time.Second, StatusInterval: 5 * time.Second,
		PendingTimeout: 4 * time.Minute, CancelGrace: 30 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h := &harness{srv: &fakeServer{status: map[string]string{}}, tok: &fakeTokens{}, drv: newFakeDriver(), now: time.Unix(1_000_000, 0)}
	c, err := New(cfg, h.srv, h.tok, h.drv, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return h.now }
	if err := c.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.c = c
	return h
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func task(id string) *daemon.Task {
	return &daemon.Task{ID: id, WorkspaceID: "ws-1", RuntimeID: "rt-1", AuthToken: "mat_" + id, RemoteMCPDaemonToken: "mdt_" + id}
}

func TestClaimCreatesPodAndKeepsCredentialsOutOfIt(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())

	name := PodName("T1")
	if _, ok := h.drv.pods[name]; !ok {
		t.Fatalf("pod %s not created", name)
	}
	if !h.tok.has("mat_T1") {
		t.Fatal("task token not registered with the relay before the pod starts")
	}
	if got := h.tok.m["mat_T1"].RemoteMCPToken; got != "mdt_T1" {
		t.Fatalf("relay must hold the per-claim MCP token, got %q", got)
	}
	in := string(h.drv.inputs[name])
	if strings.Contains(in, "mdt_T1") {
		t.Fatal("the per-claim mdt_ token must never be written into the pod input")
	}
	var parsed RunnerInput
	if err := json.Unmarshal(h.drv.inputs[name], &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Task.AuthToken != "mat_T1" || parsed.Task.RemoteMCPDaemonToken != "mat_T1" {
		t.Fatalf("input token fields wrong: %+v", parsed.Task)
	}
	if parsed.Runtime.ID != "rt-1" {
		t.Fatalf("runtime = %+v", parsed.Runtime)
	}
}

func TestClaimSizeNeverExceedsFreeCapacity(t *testing.T) {
	h := newHarness(t, nil) // MaxPods = 2
	h.srv.queue = []*daemon.Task{task("A"), task("B"), task("C")}
	h.c.Tick(context.Background())
	h.c.Tick(context.Background()) // full: must not claim at all
	if len(h.srv.claimed) != 1 || h.srv.claimed[0] != 2 {
		t.Fatalf("claim calls = %v, want exactly [2]", h.srv.claimed)
	}
	if len(h.drv.pods) != 2 {
		t.Fatalf("pods = %d, want 2", len(h.drv.pods))
	}
}

func TestPendingPodKeepsPrepareLeaseAlive(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	h.advance(16 * time.Second)
	h.c.Tick(context.Background())
	if len(h.srv.leases) != 1 || h.srv.leases[0] != "T1" {
		t.Fatalf("leases = %v", h.srv.leases)
	}
}

func TestPendingTimeoutFailsRetryablyAndCleansUp(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	h.advance(5 * time.Minute)
	h.c.Tick(context.Background())
	if h.srv.fails["T1"] != "timeout" {
		t.Fatalf("fails = %v, want timeout for T1", h.srv.fails)
	}
	if len(h.drv.pods) != 0 || h.tok.has("mat_T1") {
		t.Fatal("pod and relay token must be cleaned up")
	}
}

func TestCrashedPodFailsTaskThatIsStillRunning(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	h.drv.setPhase(PodName("T1"), PhaseRunning)
	h.srv.status["T1"] = "running"
	h.drv.setPhase(PodName("T1"), PhaseFailed)
	h.c.Tick(context.Background())
	if h.srv.fails["T1"] != "runtime_recovery" {
		t.Fatalf("fails = %v, want runtime_recovery (retryable) for T1", h.srv.fails)
	}
	if len(h.drv.pods) != 0 {
		t.Fatal("pod must be deleted")
	}
}

func TestVanishedPodFailsTaskThatIsStillRunning(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	h.srv.status["T1"] = "running"
	delete(h.drv.pods, PodName("T1"))
	h.c.Tick(context.Background())
	if h.srv.fails["T1"] != "runtime_recovery" {
		t.Fatalf("fails = %v", h.srv.fails)
	}
}

func TestCleanExitAfterRunnerReportedIsNotFailedAgain(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	h.srv.status["T1"] = "completed"
	h.drv.setPhase(PodName("T1"), PhaseSucceeded)
	h.c.Tick(context.Background())
	if _, failed := h.srv.fails["T1"]; failed {
		t.Fatal("a task the runner already completed must not be failed")
	}
	if len(h.drv.pods) != 0 || h.tok.has("mat_T1") {
		t.Fatal("pod and token must be cleaned up")
	}
}

func TestUnconfirmedStateNeverFailsOrDeletes(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	h.drv.setPhase(PodName("T1"), PhaseFailed)
	h.srv.statusErr = errors.New("server 503")
	h.c.Tick(context.Background())
	if len(h.srv.fails) != 0 {
		t.Fatalf("must not fail a task whose state is unknown: %v", h.srv.fails)
	}
	if len(h.drv.pods) != 1 {
		t.Fatal("entry must be kept for the next tick")
	}
	h.srv.statusErr = nil
	h.srv.status["T1"] = "running"
	h.c.Tick(context.Background())
	if h.srv.fails["T1"] != "runtime_recovery" {
		t.Fatalf("after recovery the task must be failed: %v", h.srv.fails)
	}
}

func TestCancelledTaskPodRemovedAfterGraceWithoutFailing(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	h.drv.setPhase(PodName("T1"), PhaseRunning)
	h.srv.status["T1"] = "cancelled"

	h.advance(6 * time.Second)
	h.c.Tick(context.Background()) // notices the cancel, starts the grace
	if len(h.drv.pods) != 1 {
		t.Fatal("the runner must get a grace period to send its cancel-ack")
	}
	h.advance(31 * time.Second)
	h.c.Tick(context.Background())
	if len(h.drv.pods) != 0 {
		t.Fatal("pod must be removed once the grace has passed")
	}
	if len(h.srv.fails) != 0 {
		t.Fatalf("a cancelled task must not be failed: %v", h.srv.fails)
	}
}

func TestCreateFailureFailsTaskAsRetryable(t *testing.T) {
	h := newHarness(t, nil)
	h.drv.createErr = errors.New("quota exceeded")
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	if h.srv.fails["T1"] != "runtime_offline" {
		t.Fatalf("fails = %v, want runtime_offline", h.srv.fails)
	}
	if h.tok.has("mat_T1") {
		t.Fatal("token must be revoked when the pod could not be created")
	}
}

func TestTaskForUnknownRuntimeIsFailedNotStranded(t *testing.T) {
	h := newHarness(t, nil)
	bad := task("T1")
	bad.RuntimeID = "rt-other"
	h.srv.queue = []*daemon.Task{bad}
	h.c.Tick(context.Background())
	if h.srv.fails["T1"] != "runtime_offline" {
		t.Fatalf("fails = %v", h.srv.fails)
	}
	if len(h.drv.pods) != 0 {
		t.Fatal("no pod for a task this controller cannot run")
	}
}

func TestAdoptionRebuildsStateWithoutRecoverOrphans(t *testing.T) {
	h := newHarness(t, nil)
	// A live pod from a previous controller instance.
	in, _ := json.Marshal(RunnerInput{Task: *task("T9"), Runtime: daemon.Runtime{ID: "rt-1"}})
	h.drv.pods[PodName("T9")] = &PodInfo{Name: PodName("T9"), Phase: PhaseRunning, Annotations: map[string]string{AnnTaskID: "T9"}}
	h.drv.inputs[PodName("T9")] = in
	// A pod whose input is gone can never talk to the relay.
	h.drv.pods["multica-task-lost"] = &PodInfo{Name: "multica-task-lost", Phase: PhaseRunning, Annotations: map[string]string{AnnTaskID: "lost"}}

	if err := h.c.adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.tok.has("mat_T9") || h.c.active() != 1 {
		t.Fatalf("live pod must be adopted: active=%d", h.c.active())
	}
	if _, ok := h.drv.pods["multica-task-lost"]; ok {
		t.Fatal("unadoptable pod must be removed")
	}
	if len(h.srv.fails) != 0 {
		t.Fatalf("adoption must not fail any task: %v", h.srv.fails)
	}
	// The adopted pod counts against capacity.
	h.srv.queue = []*daemon.Task{task("N1"), task("N2")}
	h.c.Tick(context.Background())
	if got := h.srv.claimed[len(h.srv.claimed)-1]; got != 1 {
		t.Fatalf("claim size after adopting 1 of 2 = %d, want 1", got)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}, nil, nil, nil, nil); err == nil {
		t.Fatal("empty config must be rejected")
	}
}
