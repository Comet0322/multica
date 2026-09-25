package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/k8sruntime/relay"
	"github.com/multica-ai/multica/server/pkg/agent"
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

	// runtime-request answers, keyed by "<kind>:<request id>"
	reported map[string]map[string]any
	gate     chan struct{} // when set, ReportModelListResult waits on it
	reportN  int
}

func (s *fakeServer) record(kind, id string, res map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reported == nil {
		s.reported = map[string]map[string]any{}
	}
	s.reported[kind+":"+id] = res
	s.reportN++
}

func (s *fakeServer) ReportModelListResult(_ context.Context, _, id string, res map[string]any) error {
	if s.gate != nil {
		<-s.gate
	}
	s.record("models", id, res)
	return nil
}
func (s *fakeServer) ReportLocalSkillListResult(_ context.Context, _, id string, res map[string]any) error {
	s.record("skills", id, res)
	return nil
}
func (s *fakeServer) ReportLocalSkillImportResult(_ context.Context, _, id string, res map[string]any) error {
	s.record("import", id, res)
	return nil
}
func (s *fakeServer) ReportUpdateResult(_ context.Context, _, id string, res map[string]any) error {
	s.record("update", id, res)
	return nil
}

func (s *fakeServer) waitReport(t *testing.T, key string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		r, ok := s.reported[key]
		s.mu.Unlock()
		if ok {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no answer reported for %s", key)
	return nil
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
	// noCapacity makes the next N Create calls fail with ErrNoCapacity.
	noCapacity int
	creates    int
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{pods: map[string]*PodInfo{}, inputs: map[string][]byte{}}
}
func (d *fakeDriver) Create(_ context.Context, spec PodSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.creates++
	if d.noCapacity > 0 {
		d.noCapacity--
		return fmt.Errorf("create pod: %w: exceeded quota", ErrNoCapacity)
	}
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

func TestNoCapacityHoldsTheTaskInsteadOfFailingIt(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxPods = 5 })
	h.drv.noCapacity = 1000 // the quota never frees up in this test
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())

	if len(h.srv.fails) != 0 {
		t.Fatalf("a task that cannot get a sandbox yet must not be failed (that burns its retries): %v", h.srv.fails)
	}
	if h.c.active() != 1 || !h.tok.has("mat_T1") {
		t.Fatalf("the task must be held with its relay token: active=%d", h.c.active())
	}
	// While a task is waiting, no further task is claimed even though there is
	// spare MaxPods headroom.
	h.srv.queue = []*daemon.Task{task("T2")}
	before := len(h.srv.claimed)
	h.advance(3 * time.Second)
	h.c.Tick(context.Background())
	if len(h.srv.claimed) != before {
		t.Fatal("must not claim more work while a task waits for capacity")
	}
	if h.c.active() != 1 {
		t.Fatalf("T2 must stay in the queue, active=%d", h.c.active())
	}
}

func TestWaitingTaskKeepsItsLeaseAndStartsWhenCapacityReturns(t *testing.T) {
	h := newHarness(t, nil)
	h.drv.noCapacity = 2 // the initial create and the first retry fail
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())

	h.advance(16 * time.Second)
	h.srv.status["T1"] = "dispatched"
	h.c.Tick(context.Background()) // lease refresh + retry #1 (fails)
	if len(h.srv.leases) != 1 {
		t.Fatalf("the prepare lease must be extended while waiting: %v", h.srv.leases)
	}
	h.advance(4 * time.Second)
	h.c.Tick(context.Background()) // retry #2 succeeds
	if _, ok := h.drv.pods[PodName("T1")]; !ok {
		t.Fatal("the pod must be created once capacity returns")
	}
	if len(h.srv.fails) != 0 {
		t.Fatalf("nothing should have failed: %v", h.srv.fails)
	}
	// It is now an ordinary pending pod, and claiming resumes.
	h.srv.queue = []*daemon.Task{task("T2")}
	h.advance(4 * time.Second)
	h.c.Tick(context.Background())
	if _, ok := h.drv.pods[PodName("T2")]; !ok {
		t.Fatal("claiming must resume after the waiting task got its pod")
	}
}

func TestWaitingTaskTimesOutRetryably(t *testing.T) {
	h := newHarness(t, nil)
	h.drv.noCapacity = 1000
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	h.advance(5 * time.Minute)
	h.srv.status["T1"] = "dispatched"
	h.c.Tick(context.Background())
	if h.srv.fails["T1"] != "timeout" {
		t.Fatalf("fails = %v, want a retryable timeout", h.srv.fails)
	}
	if h.c.active() != 0 || h.tok.has("mat_T1") {
		t.Fatal("entry and relay token must be cleaned up")
	}
}

func TestWaitingTaskCancelledBeforeItEverGotAPod(t *testing.T) {
	h := newHarness(t, nil)
	h.drv.noCapacity = 1000
	h.srv.queue = []*daemon.Task{task("T1")}
	h.c.Tick(context.Background())
	h.advance(6 * time.Second)
	h.srv.status["T1"] = "cancelled"
	h.c.Tick(context.Background())
	if h.c.active() != 0 {
		t.Fatal("a cancelled waiting task must be dropped at once")
	}
	if len(h.srv.fails) != 0 {
		t.Fatalf("a cancelled task must not be failed: %v", h.srv.fails)
	}
}

func modelsOf(t *testing.T, r map[string]any) []agent.Model {
	t.Helper()
	m, ok := r["models"].([]agent.Model)
	if !ok {
		t.Fatalf("models has type %T", r["models"])
	}
	return m
}

func TestModelListRequestIsAnsweredWithTheConfiguredModels(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Models = []string{"gw-large=Gateway Large", " gw-small "} })
	h.c.handleAck(context.Background(), "rt-1", &daemon.HeartbeatResponse{PendingModelList: &daemon.PendingModelList{ID: "r1"}})
	r := h.srv.waitReport(t, "models:r1")

	if r["status"] != "completed" || r["fallback"] != false || r["supported"] != true {
		t.Fatalf("answer = %v", r)
	}
	m := modelsOf(t, r)
	if len(m) != 2 || m[0].ID != "gw-large" || m[0].Label != "Gateway Large" || !m[0].Default || m[1].ID != "gw-small" || m[1].Label != "gw-small" || m[1].Default {
		t.Fatalf("models = %+v", m)
	}
}

func TestModelListFallsBackToTheBuiltInCatalogAndSaysSo(t *testing.T) {
	h := newHarness(t, nil)
	h.c.listModels = func(_ context.Context, provider string, _ agent.Command) (agent.Catalog, error) {
		if provider != "claude" {
			t.Errorf("asked about provider %q", provider)
		}
		return agent.Catalog{Models: []agent.Model{{ID: "built-in", Label: "Built in"}}, Fallback: true}, nil
	}
	h.c.handleAck(context.Background(), "rt-1", &daemon.HeartbeatResponse{PendingModelList: &daemon.PendingModelList{ID: "r2"}})
	r := h.srv.waitReport(t, "models:r2")
	if r["status"] != "completed" || r["fallback"] != true || len(modelsOf(t, r)) != 1 {
		t.Fatalf("answer = %v", r)
	}
}

func TestModelDiscoveryFailureIsReportedNotSwallowed(t *testing.T) {
	h := newHarness(t, nil)
	h.c.listModels = func(context.Context, string, agent.Command) (agent.Catalog, error) {
		return agent.Catalog{}, errors.New("boom")
	}
	h.c.handleAck(context.Background(), "rt-1", &daemon.HeartbeatResponse{PendingModelList: &daemon.PendingModelList{ID: "r3"}})
	r := h.srv.waitReport(t, "models:r3")
	if r["status"] != "failed" || r["error"] != "boom" {
		t.Fatalf("answer = %v", r)
	}
}

func TestOtherRuntimeRequestsAreAnsweredSoTheUIDoesNotWait(t *testing.T) {
	h := newHarness(t, nil)
	h.c.handleAck(context.Background(), "rt-1", &daemon.HeartbeatResponse{
		PendingLocalSkills:       &daemon.PendingLocalSkills{ID: "s1"},
		PendingUpdate:            &daemon.PendingUpdate{ID: "u1", TargetVersion: "9.9.9"},
		PendingLocalSkillImport:  &daemon.PendingLocalSkillImport{ID: "i1"},
		PendingLocalSkillImports: []daemon.PendingLocalSkillImport{{ID: "i2"}},
	})
	if r := h.srv.waitReport(t, "skills:s1"); r["status"] != "completed" || r["supported"] != false {
		t.Fatalf("skills answer = %v", r)
	}
	for _, key := range []string{"update:u1", "import:i1", "import:i2"} {
		if r := h.srv.waitReport(t, key); r["status"] != "failed" || r["error"] == "" {
			t.Fatalf("%s answer = %v", key, r)
		}
	}
}

func TestARepeatedRequestIsAnsweredOnce(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Models = []string{"m"} })
	h.srv.gate = make(chan struct{})
	ack := &daemon.HeartbeatResponse{PendingModelList: &daemon.PendingModelList{ID: "dup"}}
	h.c.handleAck(context.Background(), "rt-1", ack)
	h.c.handleAck(context.Background(), "rt-1", ack) // next heartbeat repeats it while the first is in flight
	time.Sleep(50 * time.Millisecond)
	close(h.srv.gate)
	h.srv.waitReport(t, "models:dup")
	time.Sleep(50 * time.Millisecond)
	h.srv.mu.Lock()
	n := h.srv.reportN
	h.srv.mu.Unlock()
	if n != 1 {
		t.Fatalf("answered %d times, want once", n)
	}
}
