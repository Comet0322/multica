package daemonbridge

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// classify says what a claimed task asks for.
func classify(t daemon.Task) Kind {
	switch {
	case t.IsLeaderTask || t.SquadID != "":
		return KindSquad
	case t.ChatSessionID != "":
		return KindChat
	case t.AutopilotRunID != "":
		return KindAutopilot
	case t.QuickCreatePrompt != "":
		return KindQuick
	case t.WakeupID != "":
		return KindWakeup
	default:
		return KindIssue
	}
}

// skillResolver fetches the files of skills the claim carried by reference.
type skillResolver interface {
	ResolveSkillBundle(ctx context.Context, runtimeID, taskID string, ref daemon.SkillRefData) (daemon.SkillData, daemon.TransferStats, error)
}

// BuildJob turns a claimed task into the job a backend receives. It returns a
// *RejectedError when the task needs something the backend cannot honour, so
// the task fails with a reason instead of silently running with less.
func BuildJob(ctx context.Context, task daemon.Task, caps Capabilities, resolve skillResolver) (Job, error) {
	if task.Agent == nil {
		return Job{}, Rejected(taskfailure.ReasonInvalidTaskIdentity, "claimed task carries no agent")
	}
	a := task.Agent
	if err := checkSupport(task, caps); err != nil {
		return Job{}, err
	}

	skills := append([]daemon.SkillData{}, a.Skills...)
	for _, ref := range a.SkillRefs {
		if resolve == nil {
			return Job{}, Rejected(taskfailure.ReasonSkillBundleUnavailable, "skill %q needs resolving but no resolver is configured", ref.Name)
		}
		s, _, err := resolve.ResolveSkillBundle(ctx, task.RuntimeID, task.ID, ref)
		if err != nil {
			return Job{}, Rejected(taskfailure.ReasonSkillBundleUnavailable, "resolve skill %q: %v", ref.Name, err)
		}
		skills = append(skills, s)
	}

	brief, err := daemon.RenderBrief(withSkills(task, skills), caps.Provider)
	if err != nil {
		return Job{}, Rejected(taskfailure.ReasonEnvironmentPrepareFailed, "render brief: %v", err)
	}

	raw, err := publicTask(task)
	if err != nil {
		return Job{}, err
	}

	job := Job{
		TaskID:      task.ID,
		WorkspaceID: task.WorkspaceID,
		AgentID:     task.AgentID,
		IssueID:     task.IssueID,
		Kind:        classify(task),
		System:      brief.System,
		Prompt:      brief.Prompt,
		Agent: AgentSpec{
			ID: a.ID, Name: a.Name, Instructions: a.Instructions,
			Model: a.Model, ThinkingLevel: a.ThinkingLevel, ServiceTier: a.ServiceTier,
			CustomEnv: a.CustomEnv, CustomArgs: a.CustomArgs,
			MCPConfig: a.McpConfig, RuntimeConfig: a.RuntimeConfig,
		},
		Task: raw,
	}
	for _, s := range skills {
		sk := Skill{ID: s.ID, Name: s.Name, Description: s.Description, Content: s.Content}
		for _, f := range s.Files {
			sk.Files = append(sk.Files, SkillFile{Path: f.Path, Content: f.Content})
		}
		job.Agent.Skills = append(job.Agent.Skills, sk)
	}
	if caps.Resume {
		job.ResumeSessionID = task.PriorSessionID
	}
	for _, r := range task.Repos {
		job.Repos = append(job.Repos, Repo{URL: r.URL, Description: r.Description, Ref: r.Ref})
	}
	for _, r := range task.ProjectResources {
		job.Resources = append(job.Resources, Resource{Type: r.ResourceType, Ref: r.ResourceRef, Label: r.Label})
	}
	return job, nil
}

func withSkills(t daemon.Task, skills []daemon.SkillData) daemon.Task {
	agent := *t.Agent
	agent.Skills = skills
	t.Agent = &agent
	return t
}

// checkSupport rejects a task whose agent configuration the backend would
// otherwise ignore without telling anyone.
func checkSupport(t daemon.Task, c Capabilities) error {
	a := t.Agent
	unsupported := func(what string) error {
		return Rejected(taskfailure.ReasonAgentMissingConfig,
			"agent %q uses %s, which the %s backend does not support", a.Name, what, c.Provider)
	}
	switch {
	case len(a.CustomEnv) > 0 && !c.CustomEnv:
		return unsupported("custom_env")
	case len(a.CustomArgs) > 0 && !c.CustomArgs:
		return unsupported("custom_args")
	case len(a.McpConfig) > 0 && string(a.McpConfig) != "null" && string(a.McpConfig) != "{}" && !c.MCPConfig:
		return unsupported("mcp_config")
	case len(t.RemoteMCPConnections) > 0 && !c.RemoteMCP:
		return unsupported("remote MCP connections")
	case len(t.ConnectedApps) > 0 && !c.RemoteMCP:
		return unsupported("connected apps")
	case len(a.Skills)+len(a.SkillRefs) > 0 && !c.Skills:
		return unsupported("skills")
	case a.Model != "" && !c.Model:
		return unsupported(fmt.Sprintf("model %q", a.Model))
	case (a.ThinkingLevel != "" || a.ServiceTier != "") && !c.Thinking:
		return unsupported("thinking level / service tier")
	}
	for _, r := range t.ProjectResources {
		if r.ResourceType == "local_directory" && !c.LocalWorkDir {
			return unsupported("a local_directory project resource")
		}
	}
	return nil
}

// publicTask is the claimed task as JSON with its credentials removed.
func publicTask(task daemon.Task) (json.RawMessage, error) {
	raw, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, name := range []string{"auth_token", "remote_mcp_daemon_token"} {
		delete(fields, name)
	}
	return json.Marshal(fields)
}
