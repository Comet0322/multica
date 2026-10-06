package scheduler

import (
	"context"
	"errors"
	"testing"
)

type fakeTicker struct {
	calls int
	err   error
}

func (f *fakeTicker) Tick(context.Context) error { f.calls++; return f.err }

func TestWorkflowTickJobDelegatesToTick(t *testing.T) {
	ft := &fakeTicker{}
	spec := WorkflowTickJob(ft)
	if spec.Name != "workflow_tick" {
		t.Fatalf("name = %q", spec.Name)
	}
	if _, err := spec.Handler(context.Background(), HandlerInput{}); err != nil || ft.calls != 1 {
		t.Fatalf("handler err=%v calls=%d", err, ft.calls)
	}
	ft.err = errors.New("boom")
	if _, err := spec.Handler(context.Background(), HandlerInput{}); err == nil {
		t.Fatal("handler must surface the tick error")
	}
}
