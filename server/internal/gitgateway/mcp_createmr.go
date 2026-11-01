package gitgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

const (
	toolCreateMR   = "create_merge_request"
	defaultMRLabel = "created-by-agent"
	// mrFooterMark opens the gateway's footer. The footer is always the last
	// thing in the description, so an agent that copies this format earlier in
	// its own text is still followed by the real one.
	mrFooterMark = "<!-- multica-git-gateway -->"
)

// mrRequest is a merge request the gateway has vetted.
type mrRequest struct {
	source, target, title, description string
	labels                             []string
}

// checkCreateMR applies the merge request rules after the initiator's access
// has been confirmed: the project must be open for merge requests, the source
// must be a branch this task pushed, and the target must be on the allow list.
// It returns the request to send, or the reason to refuse.
func (g *Gateway) checkCreateMR(ctx context.Context, id Identity, project string, a toolArgs) (mrRequest, string) {
	info, err := g.gitlab.projectInfo(ctx, project)
	if err != nil {
		g.log.Error("git gateway project lookup failed", "error", err)
		return mrRequest{}, "gitlab project lookup failed"
	}
	switch {
	case info.Archived:
		return mrRequest{}, "project is archived"
	case info.MRAccess == "disabled":
		return mrRequest{}, "merge requests are disabled for this project"
	}

	source := strings.TrimSpace(a.SourceBranch)
	if !slices.Contains(g.PushedBranches(id.TaskID, project), source) {
		return mrRequest{}, fmt.Sprintf("source branch %q was not pushed by this task through the gateway", source)
	}
	target := strings.TrimSpace(a.TargetBranch)
	if target == "" {
		target = info.DefaultBranch
	}
	allowed := g.cfg.MRTargets
	if len(allowed) == 0 {
		allowed = []string{info.DefaultBranch}
	}
	if target == "" || !slices.Contains(allowed, target) {
		return mrRequest{}, fmt.Sprintf("target branch %q is not allowed; allowed: %s", target, strings.Join(allowed, ", "))
	}
	return mrRequest{
		source:      source,
		target:      target,
		title:       strings.TrimSpace(a.Title),
		description: neutralizeComments(strings.TrimRight(a.Description, " \t\r\n")) + g.mrFooter(id),
		labels:      []string{g.cfg.MRLabel},
	}, ""
}

// neutralizeComments turns every HTML comment opener in agent text into an
// escaped one. An unclosed comment would otherwise swallow the footer that
// follows it and hide who the merge request is for. The footer itself is the
// only place the description may open a comment.
func neutralizeComments(s string) string {
	return strings.ReplaceAll(s, "<!--", "&lt;!--")
}

// mrFooter is what the gateway appends: who the merge request is for, which
// task made it and the fixed label. GitLab shows the admin token's owner as
// author, so this is the only record of the person.
func (g *Gateway) mrFooter(id Identity) string {
	link := id.TaskID
	if g.cfg.TaskURL != "" {
		link = strings.NewReplacer("{task_id}", id.TaskID, "{workspace_id}", id.WorkspaceID).Replace(g.cfg.TaskURL)
	}
	return fmt.Sprintf("\n\n---\n%s\nCreated by an agent through the git gateway.\n- On behalf of: %s\n- Task: %s\n- Label: `%s`\n",
		mrFooterMark, id.Initiator, link, g.cfg.MRLabel)
}

type projectInfo struct {
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	// MRAccess is GitLab's merge_requests_access_level: disabled, private or enabled.
	MRAccess string `json:"merge_requests_access_level"`
}

func (c *gitlabClient) projectInfo(ctx context.Context, project string) (projectInfo, error) {
	status, body, err := c.get(ctx, projectAPI(project), nil)
	if err != nil {
		return projectInfo{}, err
	}
	if err := apiStatus(status); err != nil {
		return projectInfo{}, err
	}
	var info projectInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return projectInfo{}, fmt.Errorf("gitlab project lookup: %w", err)
	}
	return info, nil
}

func (c *gitlabClient) createMergeRequest(ctx context.Context, project string, mr mrRequest) (any, error) {
	status, body, err := c.post(ctx, projectAPI(project)+"/merge_requests", map[string]any{
		"source_branch": mr.source,
		"target_branch": mr.target,
		"title":         mr.title,
		"description":   mr.description,
		"labels":        strings.Join(mr.labels, ","),
	})
	if err != nil {
		return nil, err
	}
	if status == http.StatusConflict {
		return nil, fmt.Errorf("gitlab refused the merge request: %s", c.scrub(oneLine(string(body))))
	}
	if err := apiStatus(status); err != nil {
		return nil, err
	}
	return pick(body, mrFields)
}
