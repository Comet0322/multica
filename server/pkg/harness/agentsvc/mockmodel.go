package agentsvc

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/multica-ai/multica/server/pkg/harness/model"
)

// MockFromScript reads a JSON array of steps and plays it as the model:
// {"say": "text"} ends the run with that answer, and
// {"tool": "bash", "input": {...}} calls a tool. It is for trying the whole
// pipeline without a model account.
func MockFromScript(path string) (model.Model, error) {
	if path == "" {
		return nil, fmt.Errorf("mock model: no script path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mock model: %w", err)
	}
	var script []struct {
		Say   string         `json:"say"`
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(raw, &script); err != nil {
		return nil, fmt.Errorf("mock model: %w", err)
	}
	steps := make([]model.Step, 0, len(script))
	for i, s := range script {
		switch {
		case s.Tool != "":
			steps = append(steps, model.Call(fmt.Sprintf("call-%d", i), s.Tool, s.Input))
		case s.Say != "":
			steps = append(steps, model.Say(s.Say))
		default:
			return nil, fmt.Errorf("mock model: step %d has neither say nor tool", i)
		}
	}
	return model.NewMock(steps...), nil
}
