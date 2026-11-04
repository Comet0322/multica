package agentsvc

import (
	"fmt"

	"github.com/multica-ai/multica/server/pkg/harness/model"
)

// ModelFactory builds Config.NewModel from settings read through get:
//
//	HARNESS_MODEL              "anthropic" or "mock"
//	HARNESS_MODEL_ID           anthropic: model id when the task names none (default claude-haiku-5-5)
//	HARNESS_ANTHROPIC_API_KEY, HARNESS_ANTHROPIC_BASE_URL
//	                           anthropic: key, and a base URL to route through a gateway such as LiteLLM
//	HARNESS_MOCK_SCRIPT        mock: path to the script (see MockFromScript)
//
// The kind is checked up front, so a service started with a typo does not come
// up and fail every task.
func ModelFactory(get func(string) string) (func(modelID string) (model.Model, error), error) {
	switch kind := get("HARNESS_MODEL"); kind {
	case "mock":
		path := get("HARNESS_MOCK_SCRIPT")
		return func(string) (model.Model, error) { return MockFromScript(path) }, nil
	case "anthropic":
		def := get("HARNESS_MODEL_ID")
		if def == "" {
			def = "claude-haiku-5-5"
		}
		return func(id string) (model.Model, error) {
			if id == "" {
				id = def
			}
			return model.NewAnthropic(model.AnthropicConfig{
				APIKey: get("HARNESS_ANTHROPIC_API_KEY"), BaseURL: get("HARNESS_ANTHROPIC_BASE_URL"), Model: id,
			})
		}, nil
	case "":
		return nil, fmt.Errorf("HARNESS_MODEL is not set")
	default:
		return nil, fmt.Errorf("model %q is not implemented (use \"anthropic\" or \"mock\")", kind)
	}
}
