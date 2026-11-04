package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Brief is the instruction text a remote executor needs for one claimed task,
// rendered by the same code the local daemon uses so the two never drift.
type Brief struct {
	// System is the runtime brief the local daemon writes to the agent's
	// CLAUDE.md / AGENTS.md: workflow, command reference, issue context.
	System string
	// Prompt is the per-run prompt the local daemon sends as the first turn.
	Prompt string
}

// ParseTask decodes a claim response task.
func ParseTask(raw []byte) (Task, error) {
	var t Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return Task{}, fmt.Errorf("decode claimed task: %w", err)
	}
	return t, nil
}

// RenderBrief renders the brief for a claimed task as provider would see it.
// It writes nothing outside a scratch directory it removes before returning.
func RenderBrief(task Task, provider string) (Brief, error) {
	dir, err := os.MkdirTemp("", "multica-brief-")
	if err != nil {
		return Brief{}, err
	}
	defer os.RemoveAll(dir)
	system, err := execenv.InjectRuntimeConfig(dir, provider, briefTaskContext(task, provider))
	if err != nil {
		return Brief{}, fmt.Errorf("render runtime brief: %w", err)
	}
	return Brief{System: strings.TrimSpace(system), Prompt: BuildPrompt(task, provider)}, nil
}

// briefTaskContext mirrors the context runTask hands execenv, minus everything
// that describes a local checkout.
func briefTaskContext(task Task, provider string) execenv.TaskContextForEnv {
	var (
		agentName, instructions string
		skills                  []SkillData
	)
	if task.Agent != nil {
		agentName, instructions, skills = task.Agent.Name, task.Agent.Instructions, task.Agent.Skills
	}
	return execenv.TaskContextForEnv{
		IssueID:                          task.IssueID,
		TriggerCommentID:                 task.TriggerCommentID,
		TriggerThreadID:                  task.TriggerThreadID,
		CommentReplyTargets:              commentReplyThreads(task),
		NewCommentCount:                  task.NewCommentCount,
		NewCommentsSince:                 task.NewCommentsSince,
		PriorSessionResumed:              task.PriorSessionID != "",
		PriorSessionResumeUnavailable:    task.PriorSessionResumeUnavailable,
		AgentID:                          task.AgentID,
		AgentName:                        agentName,
		AgentInstructions:                instructions,
		AgentSkills:                      convertSkillsForEnv(skills),
		DisabledRuntimeSkills:            convertDisabledRuntimeSkillsForEnv(task.Agent, task.RuntimeID, provider),
		Repos:                            convertReposForEnv(task.Repos),
		ProjectID:                        task.ProjectID,
		ProjectTitle:                     task.ProjectTitle,
		ProjectDescription:               task.ProjectDescription,
		ProjectResources:                 convertProjectResourcesForEnv(task.ProjectResources),
		ChatSessionID:                    task.ChatSessionID,
		ChatChannelType:                  task.ChatChannelType,
		ChatChannelDeliversFiles:         task.ChatChannelDeliversFiles,
		AutopilotRunID:                   task.AutopilotRunID,
		AutopilotID:                      task.AutopilotID,
		AutopilotTitle:                   task.AutopilotTitle,
		AutopilotDescription:             task.AutopilotDescription,
		AutopilotSource:                  task.AutopilotSource,
		AutopilotTriggerPayload:          strings.TrimSpace(string(task.AutopilotTriggerPayload)),
		QuickCreatePrompt:                task.QuickCreatePrompt,
		IsSquadLeader:                    taskIsSquadLeader(task),
		RequestingUserName:               task.RequestingUserName,
		RequestingUserProfileDescription: task.RequestingUserProfileDescription,
		InitiatorType:                    task.InitiatorType,
		InitiatorID:                      task.InitiatorID,
		InitiatorName:                    task.InitiatorName,
		InitiatorEmail:                   task.InitiatorEmail,
		WorkspaceContext:                 task.WorkspaceContext,
		IssueStatuses:                    convertIssueStatusesForEnv(task.IssueStatuses),
		IssueStatusesOmitted:             task.IssueStatusesOmitted,
		ConnectedApps:                    task.ConnectedApps,
	}
}

// CLIEnv is the environment the multica CLI needs to act as the task's agent,
// as the local daemon injects it into a spawned agent. configRoot must be an
// empty private directory so the CLI never reads a user's own profile.
func CLIEnv(task Task, serverURL, configRoot, tempDir string) (map[string]string, error) {
	scoped, err := taskScopedAuthToken(task)
	if err != nil {
		return nil, err
	}
	agentName := ""
	if task.Agent != nil {
		agentName = task.Agent.Name
	}
	env := taskMulticaEnvironment(task, "", agentName, scoped, configRoot, "", serverURL, 0, 0, tempDir)
	if task.AutopilotRunID != "" {
		env["MULTICA_AUTOPILOT_RUN_ID"] = task.AutopilotRunID
	}
	if task.AutopilotID != "" {
		env["MULTICA_AUTOPILOT_ID"] = task.AutopilotID
	}
	if task.QuickCreatePrompt != "" {
		env["MULTICA_QUICK_CREATE_TASK_ID"] = task.ID
		if len(task.QuickCreateAttachmentIDs) > 0 {
			if raw, err := json.Marshal(task.QuickCreateAttachmentIDs); err == nil {
				env["MULTICA_QUICK_CREATE_ATTACHMENT_IDS"] = string(raw)
			}
		}
	}
	return env, nil
}
