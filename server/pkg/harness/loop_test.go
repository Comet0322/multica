package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/harness"
	"github.com/multica-ai/multica/server/pkg/harness/model"
	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
	"github.com/multica-ai/multica/server/pkg/harness/tools"
)

func registry(t *testing.T) *tools.Registry {
	t.Helper()
	sb, err := sandbox.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Put(context.Background(), "/w/.keep", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return tools.NewRegistry(tools.Sandboxed{SB: sb, Workdir: "/w"}.Tools()...)
}

func collect() (*[]harness.Event, func(harness.Event)) {
	var evs []harness.Event
	return &evs, func(e harness.Event) { evs = append(evs, e) }
}

func TestRunWorksThroughToolsToTheFinalAnswer(t *testing.T) {
	m := model.NewMock(
		model.Call("c1", "write_file", map[string]any{"path": "hello.txt", "content": "hi there\n"}),
		model.Call("c2", "bash", map[string]any{"command": "wc -c < hello.txt"}),
		model.Say("the file has 9 bytes"),
	)
	evs, emit := collect()

	out := harness.Run(context.Background(), harness.Config{Model: m, Tools: registry(t), System: "sys", Emit: emit}, "make a file")

	if out.Status != harness.StatusCompleted || out.Output != "the file has 9 bytes" || out.Turns != 3 {
		t.Fatalf("outcome = %+v", out)
	}
	var kinds []string
	for _, e := range *evs {
		kinds = append(kinds, string(e.Kind))
	}
	if got := strings.Join(kinds, ","); got != "tool-use,tool-result,tool-use,tool-result,text" {
		t.Errorf("event order = %s", got)
	}
	if u := out.Usage["mock"]; u.InputTokens != 30 || u.OutputTokens != 15 {
		t.Errorf("usage = %+v, want 3 turns of 10/5", u)
	}

	// The model must have been shown the prompt, the system prompt, every tool,
	// and each tool result before its next turn.
	reqs := m.Requests()
	if reqs[0].System != "sys" || reqs[0].Messages[0].Text != "make a file" || len(reqs[0].Tools) != 5 {
		t.Errorf("first request = %+v", reqs[0])
	}
	last := reqs[2].Messages
	if res := last[len(last)-1].ToolResults; len(res) != 1 || strings.TrimSpace(res[0].Output) != "9" {
		t.Errorf("model was not shown the bash result: %+v", last[len(last)-1])
	}
}

func TestAToolMistakeGoesBackToTheModelInsteadOfEndingTheRun(t *testing.T) {
	m := model.NewMock(
		model.Call("c1", "no_such_tool", nil),
		model.Call("c2", "read_file", map[string]any{"path": "missing"}),
		model.Say("recovered"),
	)
	out := harness.Run(context.Background(), harness.Config{Model: m, Tools: registry(t)}, "go")
	if out.Status != harness.StatusCompleted || out.Output != "recovered" {
		t.Fatalf("outcome = %+v", out)
	}
	last := m.Requests()[2].Messages
	if r := last[len(last)-1].ToolResults[0]; !r.IsError || !strings.Contains(r.Output, "does not exist") {
		t.Errorf("the model should have been told the file is missing: %+v", r)
	}
}

func TestRunStopsAtTheTurnLimit(t *testing.T) {
	var steps []model.Step
	for range 10 {
		steps = append(steps, model.Call("c", "bash", map[string]any{"command": "true"}))
	}
	out := harness.Run(context.Background(), harness.Config{Model: model.NewMock(steps...), Tools: registry(t), MaxTurns: 3}, "loop forever")
	if out.Status != harness.StatusFailed || out.Turns != 3 || !strings.Contains(out.Error, "3 turns") {
		t.Errorf("outcome = %+v", out)
	}
}

func TestRunReportsCancellationAsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := harness.Run(ctx, harness.Config{Model: model.NewMock(model.Say("never")), Tools: registry(t)}, "go")
	if out.Status != harness.StatusCancelled {
		t.Errorf("outcome = %+v", out)
	}
}

type failingModel struct{ err error }

func (f failingModel) Next(context.Context, model.Request) (model.Turn, error) {
	return model.Turn{}, f.err
}

func TestAModelFailureFailsTheRunAndSaysSo(t *testing.T) {
	evs, emit := collect()
	out := harness.Run(context.Background(), harness.Config{Model: failingModel{errors.New("429 rate limited")}, Tools: registry(t), Emit: emit}, "go")
	if out.Status != harness.StatusFailed || !strings.Contains(out.Error, "429") {
		t.Fatalf("outcome = %+v", out)
	}
	if len(*evs) != 1 || (*evs)[0].Kind != harness.EventError {
		t.Errorf("an error event should be emitted: %+v", *evs)
	}
}

func TestExhaustedScriptIsAFailureNotAHang(t *testing.T) {
	out := harness.Run(context.Background(), harness.Config{Model: model.NewMock(), Tools: registry(t)}, "go")
	if out.Status != harness.StatusFailed {
		t.Errorf("outcome = %+v", out)
	}
}
