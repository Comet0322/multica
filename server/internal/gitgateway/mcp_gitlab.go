package gitgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// post sends a JSON body to GitLab's API with the admin token.
func (c *gitlabClient) post(ctx context.Context, path string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v4"+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", c.ticket)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, redact(err, c.ticket)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, redact(err, c.ticket)
	}
	return resp.StatusCode, out, nil
}

// pick keeps only the named fields of each object, so the agent sees a small,
// stable shape and nothing GitLab adds later. An author object is reduced to
// its username.
func pick(body []byte, fields []string) (any, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	one := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for _, f := range fields {
			if val, ok := m[f]; ok {
				out[f] = val
			}
		}
		if a, ok := m["author"].(map[string]any); ok {
			out["author"] = a["username"]
		}
		return out
	}
	switch t := v.(type) {
	case []any:
		list := make([]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				list = append(list, one(m))
			}
		}
		return list, nil
	case map[string]any:
		return one(t), nil
	}
	return nil, errors.New("unexpected gitlab reply")
}

var (
	branchFields = []string{"name", "default", "protected", "merged", "web_url"}
	mrFields     = []string{"iid", "title", "state", "draft", "description", "source_branch", "target_branch", "merge_status", "web_url", "created_at", "updated_at"}
	noteFields   = []string{"id", "body", "created_at"}
)

func projectAPI(project string) string { return "/projects/" + url.PathEscape(project) }

func (c *gitlabClient) listBranches(ctx context.Context, project, search string) (any, error) {
	q := url.Values{"per_page": {"100"}}
	if search != "" {
		q.Set("search", search)
	}
	return c.fetch(ctx, projectAPI(project)+"/repository/branches", q, branchFields)
}

func (c *gitlabClient) getMergeRequest(ctx context.Context, project string, iid int) (any, error) {
	return c.fetch(ctx, projectAPI(project)+"/merge_requests/"+strconv.Itoa(iid), nil, mrFields)
}

func (c *gitlabClient) listMergeRequests(ctx context.Context, project, state string) (any, error) {
	return c.fetch(ctx, projectAPI(project)+"/merge_requests", url.Values{"state": {state}, "per_page": {"50"}}, mrFields)
}

func (c *gitlabClient) fetch(ctx context.Context, path string, q url.Values, fields []string) (any, error) {
	status, body, err := c.get(ctx, path, q)
	if err != nil {
		return nil, err
	}
	if err := apiStatus(status); err != nil {
		return nil, err
	}
	return pick(body, fields)
}

func (c *gitlabClient) addMergeRequestNote(ctx context.Context, project string, iid int, text string) (any, error) {
	status, body, err := c.post(ctx, projectAPI(project)+"/merge_requests/"+strconv.Itoa(iid)+"/notes", map[string]string{"body": text})
	if err != nil {
		return nil, err
	}
	if err := apiStatus(status); err != nil {
		return nil, err
	}
	return pick(body, noteFields)
}

// errNotFound is a GitLab 404: the agent named something that does not exist,
// which is not an outage.
var errNotFound = errors.New("not found")

func apiStatus(status int) error {
	switch {
	case status == http.StatusNotFound:
		return errNotFound
	case status >= 200 && status < 300:
		return nil
	}
	return fmt.Errorf("gitlab answered %d", status)
}
