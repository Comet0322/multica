package runtimecontroller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

type fakeServer struct {
	mu       sync.Mutex
	runtimes []Runtime
	issued   []string // daemon ids, in order
	revoked  []string
	reports  map[string]string
	listErr  error
	tokenSeq int
	ttls     []time.Duration
	now      func() time.Time
}

func (f *fakeServer) ManagedRuntimes(context.Context) ([]Runtime, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Runtime(nil), f.runtimes...), nil
}

func (f *fakeServer) IssueDaemonToken(_ context.Context, ws, daemon string, ttl time.Duration) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenSeq++
	f.issued = append(f.issued, daemon)
	f.ttls = append(f.ttls, ttl)
	return Token{Value: fmt.Sprintf("mdt_%d", f.tokenSeq), ExpiresAt: f.now().Add(ttl)}, nil
}

func (f *fakeServer) RevokeDaemonTokens(_ context.Context, ws, daemon string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, ws+"/"+daemon)
	return nil
}

func (f *fakeServer) ReportStates(_ context.Context, states []StateReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reports == nil {
		f.reports = map[string]string{}
	}
	for _, s := range states {
		f.reports[s.DaemonID] = s.State
	}
	return nil
}

func (f *fakeServer) set(rt ...Runtime) { f.mu.Lock(); f.runtimes = rt; f.mu.Unlock() }

type fakeKube struct {
	mu        sync.Mutex
	sandboxes map[string]*SandboxInfo
	specs     map[string]SandboxSpec
	secrets   map[string]*SecretInfo
	modeCalls []string
	failPut   map[string]error
}

func newFakeKube() *fakeKube {
	return &fakeKube{sandboxes: map[string]*SandboxInfo{}, specs: map[string]SandboxSpec{}, secrets: map[string]*SecretInfo{}, failPut: map[string]error{}}
}

func (k *fakeKube) GetSandbox(_ context.Context, name string) (*SandboxInfo, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s := k.sandboxes[name]; s != nil {
		c := *s
		return &c, nil
	}
	return nil, nil
}

func (k *fakeKube) ListManagedSandboxes(context.Context) ([]SandboxInfo, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []SandboxInfo
	for _, s := range k.sandboxes {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (k *fakeKube) CreateSandbox(_ context.Context, s SandboxSpec) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.sandboxes[s.Name] = &SandboxInfo{Name: s.Name, Mode: ModeRunning, Labels: map[string]string{LabelWorkspaceID: s.WorkspaceID, LabelDaemonID: s.DaemonID, LabelManagedBy: ManagedByValue}}
	k.specs[s.Name] = s
	return nil
}

func (k *fakeKube) SetMode(_ context.Context, name, mode string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.sandboxes[name] == nil {
		return errors.New("not found")
	}
	k.sandboxes[name].Mode = mode
	k.modeCalls = append(k.modeCalls, name+"="+mode)
	return nil
}

func (k *fakeKube) DeleteSandbox(_ context.Context, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.sandboxes, name)
	return nil
}

func (k *fakeKube) GetSecret(_ context.Context, name string) (*SecretInfo, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s := k.secrets[name]; s != nil {
		c := *s
		return &c, nil
	}
	return nil, nil
}

func (k *fakeKube) PutSecret(_ context.Context, name string, _, annotations, data map[string]string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.failPut[name]; err != nil {
		return err
	}
	k.secrets[name] = &SecretInfo{Data: data, Annotations: annotations}
	return nil
}

func (k *fakeKube) DeleteSecret(_ context.Context, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.secrets, name)
	return nil
}

type fakeLLM struct {
	mu      sync.Mutex
	seq     int
	live    map[string]LLMKeyRequest
	deleted []string
}

func newFakeLLM() *fakeLLM { return &fakeLLM{live: map[string]LLMKeyRequest{}} }

func (l *fakeLLM) Generate(_ context.Context, r LLMKeyRequest) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	k := fmt.Sprintf("sk-fake-%d", l.seq)
	l.live[k] = r
	return k, nil
}

func (l *fakeLLM) Delete(_ context.Context, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.live, key)
	l.deleted = append(l.deleted, key)
	return nil
}
