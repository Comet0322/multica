package gitgateway

import (
	"encoding/json"
	"errors"
	"strings"
)

// MCPServerName is the key of the gateway's entry in agent.mcp_config.
const MCPServerName = "gitlab-gateway"

// InjectMCPConfig returns the claimed task body with the gateway's http MCP
// server written into agent.mcp_config.mcpServers, carrying the task's token.
// The agent's own servers stay; any entry whose name matches the gateway's
// (ignoring case) is dropped, so the user cannot shadow it.
func InjectMCPConfig(task json.RawMessage, mcpURL, ticket string) (json.RawMessage, error) {
	if mcpURL == "" || ticket == "" {
		return nil, errors.New("gitgateway: MCP URL and token are required")
	}
	var t map[string]json.RawMessage
	if err := json.Unmarshal(task, &t); err != nil {
		return nil, err
	}
	agent := map[string]json.RawMessage{}
	if a := t["agent"]; len(a) > 0 && string(a) != "null" {
		if err := json.Unmarshal(a, &agent); err != nil {
			return nil, err
		}
	}
	cfg := map[string]json.RawMessage{}
	if c := agent["mcp_config"]; len(c) > 0 && string(c) != "null" {
		if err := json.Unmarshal(c, &cfg); err != nil {
			return nil, err
		}
	}
	servers := map[string]json.RawMessage{}
	if s := cfg["mcpServers"]; len(s) > 0 && string(s) != "null" {
		if err := json.Unmarshal(s, &servers); err != nil {
			return nil, err
		}
	}
	for name := range servers {
		if strings.EqualFold(name, MCPServerName) {
			delete(servers, name)
		}
	}
	entry, err := json.Marshal(map[string]any{
		"type":    "http",
		"url":     mcpURL,
		"headers": map[string]string{"Authorization": "Bearer " + ticket},
	})
	if err != nil {
		return nil, err
	}
	servers[MCPServerName] = entry
	if cfg["mcpServers"], err = json.Marshal(servers); err != nil {
		return nil, err
	}
	if agent["mcp_config"], err = json.Marshal(cfg); err != nil {
		return nil, err
	}
	if t["agent"], err = json.Marshal(agent); err != nil {
		return nil, err
	}
	return json.Marshal(t)
}
