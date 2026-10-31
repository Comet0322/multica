package gitgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitLab access levels.
const (
	LevelReporter  = 20
	LevelDeveloper = 30
)

// gitlabUser is the part of a GitLab user the gateway needs.
type gitlabUser struct {
	ID     int64
	Active bool
	Found  bool
}

// gitlabClient asks GitLab about people and their access. Every failure is an
// error and is never cached, so the caller can only refuse.
type gitlabClient struct {
	base  string
	token string
	http  *http.Client
}

func (c *gitlabClient) get(ctx context.Context, path string, query url.Values) (int, []byte, error) {
	u := c.base + "/api/v4" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, redact(err, c.token)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, redact(err, c.token)
	}
	return resp.StatusCode, body, nil
}

// redact keeps the admin token out of error text.
func redact(err error, token string) error {
	if token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "[redacted]"))
}

// scrub keeps the admin token out of text taken from one of GitLab's answers,
// which can echo what it was sent. Such text is bounded as well.
func (c *gitlabClient) scrub(s string) string {
	if c.token != "" {
		s = strings.ReplaceAll(s, c.token, "[redacted]")
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// findUser looks a user up by exact email. The admin search also returns
// partial matches, so the email is compared again here.
func (c *gitlabClient) findUser(ctx context.Context, email string) (gitlabUser, error) {
	status, body, err := c.get(ctx, "/users", url.Values{"search": {email}})
	if err != nil {
		return gitlabUser{}, err
	}
	if status != http.StatusOK {
		return gitlabUser{}, fmt.Errorf("gitlab users lookup: status %d", status)
	}
	var users []struct {
		ID          int64  `json:"id"`
		State       string `json:"state"`
		Email       string `json:"email"`
		PublicEmail string `json:"public_email"`
	}
	if err := json.Unmarshal(body, &users); err != nil {
		return gitlabUser{}, fmt.Errorf("gitlab users lookup: %w", err)
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, email) || strings.EqualFold(u.PublicEmail, email) {
			return gitlabUser{ID: u.ID, Active: u.State == "active", Found: true}, nil
		}
	}
	return gitlabUser{}, nil
}

// accessLevel is the user's effective level in the project, group inheritance
// included. A user with no access gets 0.
func (c *gitlabClient) accessLevel(ctx context.Context, project string, userID int64) (int, error) {
	status, body, err := c.get(ctx, "/projects/"+url.PathEscape(project)+"/members/all/"+strconv.FormatInt(userID, 10), nil)
	if err != nil {
		return 0, err
	}
	switch status {
	case http.StatusNotFound:
		return 0, nil
	case http.StatusOK:
	default:
		return 0, fmt.Errorf("gitlab member lookup: status %d", status)
	}
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return 0, fmt.Errorf("gitlab member lookup: %w", err)
	}
	return m.AccessLevel, nil
}

// ttlCache holds successful answers for a fixed time. An expired entry is a
// miss; stale values are never served.
type ttlCache[V any] struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]ttlEntry[V]
}

type ttlEntry[V any] struct {
	v       V
	expires time.Time
}

func newTTLCache[V any](ttl time.Duration, now func() time.Time) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, now: now, m: map[string]ttlEntry[V]{}}
}

func (c *ttlCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || !c.now().Before(e.expires) {
		delete(c.m, key)
		var zero V
		return zero, false
	}
	return e.v, true
}

func (c *ttlCache[V]) put(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = ttlEntry[V]{v: v, expires: c.now().Add(c.ttl)}
}
