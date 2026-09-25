package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/pkg/agent"
)

const (
	maxModelPages  = 10
	maxModels      = 1000
	maxModelBodyMB = 4
)

// gatewayModelsPage is one page of a gateway's model list. Both the Anthropic
// Models API and OpenAI-compatible gateways answer with {"data":[{"id":...}]};
// only the Anthropic one adds display_name and pagination.
type gatewayModelsPage struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

// scanGatewayModels asks a gateway which models it serves (GET <base>/v1/models).
// base is the same value as the agents' ANTHROPIC_BASE_URL. The key, when given,
// is sent both as a bearer token and as x-api-key so either kind of gateway
// accepts it. Nothing about the key is ever returned in an error.
func scanGatewayModels(ctx context.Context, client *http.Client, base, key string) ([]agent.Model, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if _, err := url.ParseRequestURI(base); err != nil {
		return nil, fmt.Errorf("invalid models URL %q", base)
	}
	var (
		models []agent.Model
		seen   = map[string]struct{}{}
		after  string
	)
	for page := 0; page < maxModelPages; page++ {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("x-api-key", key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("scan %s/v1/models: %w", base, redactURLError(err))
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxModelBodyMB<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("scan %s/v1/models: %w", base, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("scan %s/v1/models: HTTP %d", base, resp.StatusCode)
		}
		var p gatewayModelsPage
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("scan %s/v1/models: the reply is not a model list", base)
		}
		for _, m := range p.Data {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			label := strings.TrimSpace(m.DisplayName)
			if label == "" {
				label = id
			}
			models = append(models, agent.Model{ID: id, Label: label})
			if len(models) >= maxModels {
				return models, nil
			}
		}
		if !p.HasMore || p.LastID == "" {
			break
		}
		after = p.LastID
	}
	if len(models) == 0 {
		return nil, errors.New("the gateway returned no models")
	}
	return models, nil
}

// redactURLError drops the request URL from a transport error so a query string
// or credential can never reach a log or the UI.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// parseConfiguredModels turns "id" / "id=Label" entries into models.
func parseConfiguredModels(specs []string) []agent.Model {
	models := make([]agent.Model, 0, len(specs))
	for _, spec := range specs {
		id, label, _ := strings.Cut(spec, "=")
		id, label = strings.TrimSpace(id), strings.TrimSpace(label)
		if id == "" {
			continue
		}
		if label == "" {
			label = id
		}
		models = append(models, agent.Model{ID: id, Label: label})
	}
	return models
}

// markDefault flags one model as the advertised pick: the configured default if
// it is in the list, otherwise the first.
func markDefault(models []agent.Model, preferred string) {
	pick := 0
	for i, m := range models {
		if m.ID == preferred {
			pick = i
			break
		}
	}
	for i := range models {
		models[i].Default = i == pick
	}
}
