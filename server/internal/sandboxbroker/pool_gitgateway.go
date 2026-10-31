package sandboxbroker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

// Placeholders usable in GitGatewayConfig.Env values.
const (
	// GitTokenPlaceholder is replaced by the task's git gateway token.
	GitTokenPlaceholder = "${GIT_TOKEN}"
	// GitURLPlaceholder is replaced by GitGatewayConfig.URL.
	GitURLPlaceholder = "${GIT_GATEWAY_URL}"
)

// GitGatewayConfig makes the pool manager give every task a token that is
// valid only at the git gateway, and answer the gateway's questions about
// tokens and pod addresses. The token is revoked when the lease ends.
type GitGatewayConfig struct {
	// Registry holds the live tokens. Nil creates one. Share it only with
	// code that serves the gateway's verification endpoints.
	Registry *gitgateway.TokenRegistry
	// URL is the gateway base URL as pods reach it; it fills
	// GitURLPlaceholder.
	URL string
	// MCPURL, when set, is the gateway's MCP endpoint as agents reach it. Every
	// task then gets a "gitlab-gateway" http MCP server carrying its token.
	// Empty leaves the agent's MCP configuration untouched.
	MCPURL string
	// Env lists the agent environment variables the manager forces, after the
	// agent's own settings are merged. Values may use the placeholders above.
	// Names must not start with MULTICA_. Empty selects DefaultGitGatewayEnv.
	Env map[string]string
}

// DefaultGitGatewayEnv hands the token to the image's git credential setup.
var DefaultGitGatewayEnv = map[string]string{
	"GIT_GATEWAY_TOKEN": GitTokenPlaceholder,
}

func (c *GitGatewayConfig) normalize() error {
	if c.URL != "" {
		if err := checkBaseURL(c.URL); err != nil {
			return fmt.Errorf("git gateway URL: %w", err)
		}
		c.URL = strings.TrimRight(c.URL, "/")
	}
	if c.MCPURL != "" {
		if err := checkBaseURL(c.MCPURL); err != nil {
			return fmt.Errorf("git gateway MCP URL: %w", err)
		}
	}
	if c.Registry == nil {
		c.Registry = gitgateway.NewTokenRegistry()
	}
	if len(c.Env) == 0 {
		c.Env = DefaultGitGatewayEnv
	}
	for name := range c.Env {
		if name == "" || strings.HasPrefix(strings.ToUpper(name), "MULTICA_") {
			return fmt.Errorf("git gateway env name %q must be set and must not start with MULTICA_", name)
		}
	}
	return nil
}

// mountGit adds the endpoints the git gateway calls. They belong on the admin
// listener, which pods cannot reach.
func (p *Pool) mountGit(mux *http.ServeMux) {
	if p.cfg.Git == nil {
		return
	}
	mux.Handle("POST /v1/git/verify-token", p.cfg.Git.Registry.VerifyHandler())
	mux.Handle("POST /v1/git/verify-pod", gitgateway.PodVerifyHandler(p.gitIdentitiesForIP))
}

// gitIdentitiesForIP lists the tasks leased to the live pod at ip. A pod that
// is idle, resetting, lost or unknown holds none, so its daemon reads nothing.
func (p *Pool) gitIdentitiesForIP(ip string) []gitgateway.Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markLostLocked()
	pd := p.pods[ip]
	if pd == nil || pd.State != PodLeased {
		return nil
	}
	ids := make([]gitgateway.Identity, 0, len(pd.leases))
	for _, l := range pd.leases {
		ids = append(ids, gitgateway.Identity{TaskID: l.TaskID, WorkspaceID: l.WorkspaceID, Initiator: l.Initiator})
	}
	return ids
}

// issueGitToken gives the claimed task a gateway token and returns the task
// body with the configured environment injected.
func (p *Pool) issueGitToken(raw json.RawMessage, ref claimedTaskRef) (json.RawMessage, error) {
	token, err := p.cfg.Git.Registry.Issue(gitgateway.Identity{TaskID: ref.ID, WorkspaceID: ref.WorkspaceID, Initiator: ref.initiator()})
	if err != nil {
		return nil, err
	}
	repl := strings.NewReplacer(GitTokenPlaceholder, token, GitURLPlaceholder, p.cfg.Git.URL)
	env := make(map[string]string, len(p.cfg.Git.Env))
	for name, value := range p.cfg.Git.Env {
		env[name] = repl.Replace(value)
	}
	out, err := forceAgentEnv(raw, env)
	if err == nil && p.cfg.Git.MCPURL != "" {
		out, err = gitgateway.InjectMCPConfig(out, p.cfg.Git.MCPURL, token)
	}
	if err != nil {
		p.cfg.Git.Registry.Revoke(ref.ID)
		return nil, err
	}
	return out, nil
}

// revokeGit ends the task's gateway token. It is safe to call without one.
func (p *Pool) revokeGit(taskID string) {
	if p.cfg.Git != nil {
		p.cfg.Git.Registry.Revoke(taskID)
	}
}

// forceAgentEnv returns the claimed task with env set in agent.custom_env
// after whatever the agent configured, so the user cannot override it.
func forceAgentEnv(raw json.RawMessage, forced map[string]string) (json.RawMessage, error) {
	var task map[string]json.RawMessage
	if err := json.Unmarshal(raw, &task); err != nil {
		return nil, err
	}
	var agent map[string]json.RawMessage
	if a := task["agent"]; len(a) > 0 && string(a) != "null" {
		if err := json.Unmarshal(a, &agent); err != nil {
			return nil, err
		}
	}
	if agent == nil {
		agent = map[string]json.RawMessage{}
	}
	env := map[string]string{}
	if e := agent["custom_env"]; len(e) > 0 && string(e) != "null" {
		if err := json.Unmarshal(e, &env); err != nil {
			return nil, err
		}
	}
	for name, value := range forced {
		env[name] = value
	}
	var err error
	if agent["custom_env"], err = json.Marshal(env); err != nil {
		return nil, err
	}
	if task["agent"], err = json.Marshal(agent); err != nil {
		return nil, err
	}
	return json.Marshal(task)
}
