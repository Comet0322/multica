package gitgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	defaultCacheTTL      = 10 * time.Minute
	defaultLookupTimeout = 10 * time.Second
)

// Config configures a Gateway.
type Config struct {
	// GitLabURL is the GitLab base URL, e.g. https://gitlab.example.com.
	GitLabURL string
	// AdminToken is the GitLab administrator token. It stays in this process.
	AdminToken string
	// Resolver answers which task a token belongs to.
	Resolver Resolver
	// PodResolver, when set, lets a request without a task token be
	// identified by its source IP, for reads only (the daemon's own clones
	// and fetches). Nil refuses every request without a token.
	PodResolver PodResolver
	// MinReadLevel is the lowest GitLab access level allowed to clone and
	// fetch; default Reporter.
	MinReadLevel int
	// CacheTTL bounds how long user and access answers are reused; default
	// 10 minutes.
	CacheTTL time.Duration
	// LookupTimeout bounds each call to GitLab's API; default 10 seconds.
	LookupTimeout time.Duration
	// Audit receives one JSON line per decision.
	Audit io.Writer

	HTTPClient *http.Client // GitLab API calls; default has LookupTimeout
	Transport  http.RoundTripper
	Logger     *slog.Logger
	Now        func() time.Time
}

// Gateway serves git smart HTTP reads (clone, fetch) for task tokens.
type Gateway struct {
	cfg    Config
	gitlab *gitlabClient
	base   *url.URL
	users  *ttlCache[gitlabUser]
	levels *ttlCache[int]
	audit  *auditLog
	proxy  *httputil.ReverseProxy
	log    *slog.Logger
	now    func() time.Time
}

func New(cfg Config) (*Gateway, error) {
	base, err := url.Parse(strings.TrimRight(cfg.GitLabURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("gitgateway: GitLabURL must be an http(s) URL")
	}
	if cfg.AdminToken == "" {
		return nil, errors.New("gitgateway: AdminToken is required")
	}
	if cfg.Resolver == nil {
		return nil, errors.New("gitgateway: Resolver is required")
	}
	if cfg.MinReadLevel == 0 {
		cfg.MinReadLevel = LevelReporter
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = defaultCacheTTL
	}
	if cfg.LookupTimeout == 0 {
		cfg.LookupTimeout = defaultLookupTimeout
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.LookupTimeout}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	g := &Gateway{
		cfg:    cfg,
		gitlab: &gitlabClient{base: base.String(), token: cfg.AdminToken, http: cfg.HTTPClient},
		base:   base,
		users:  newTTLCache[gitlabUser](cfg.CacheTTL, now),
		levels: newTTLCache[int](cfg.CacheTTL, now),
		log:    cfg.Logger,
		now:    now,
	}
	g.audit = &auditLog{w: cfg.Audit, err: func(err error) { g.log.Error("git gateway audit write failed", "error", err) }}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:   g.rewrite,
		Transport: cfg.Transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			g.log.Error("git gateway upstream failed", "error", redact(err, cfg.AdminToken))
			http.Error(w, "git upstream unavailable", http.StatusBadGateway)
		},
	}
	return g, nil
}

type routeKey struct{}

// rewrite builds the GitLab request from the vetted route alone. The client's
// credentials never go upstream; the admin token replaces them.
func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	rt := pr.In.Context().Value(routeKey{}).(route)
	out := pr.Out
	out.URL.Scheme = g.base.Scheme
	out.URL.Host = g.base.Host
	out.URL.Path = g.base.Path + rt.path
	out.URL.RawPath = ""
	out.URL.RawQuery = rt.query
	out.Host = g.base.Host
	out.Header.Del("Authorization")
	out.Header.Del("Cookie")
	out.SetBasicAuth("git-gateway", g.cfg.AdminToken)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := bearerOrBasicPassword(r)
	if token == "" {
		if g.cfg.PodResolver != nil {
			g.serveDaemonRead(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git gateway"`)
		http.Error(w, "task token required", http.StatusUnauthorized)
		return
	}
	id, err := g.cfg.Resolver.Resolve(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnknownToken):
		g.deny(w, r, AuditRecord{Action: "git.request", Reason: "unknown or revoked task token"}, http.StatusUnauthorized)
		return
	case err != nil:
		g.log.Error("git gateway token verification failed", "error", err)
		g.deny(w, r, AuditRecord{Action: "git.request", Reason: "task token verification failed"}, http.StatusServiceUnavailable)
		return
	}
	rec := AuditRecord{TaskID: id.TaskID, WorkspaceID: id.WorkspaceID, Initiator: id.Initiator}

	rt, why := parseRoute(r.Method, r.URL)
	if why != "" {
		rec.Action, rec.Reason = "git.request", why
		g.deny(w, r, rec, http.StatusForbidden)
		return
	}
	rec.Action, rec.Project = rt.action, rt.project

	status, reason := g.authorizeRead(r.Context(), id.Initiator, rt.project)
	if status != 0 {
		rec.Reason = reason
		g.deny(w, r, rec, status)
		return
	}
	rec.Decision, rec.Reason, rec.Time = AuditAllow, fmt.Sprintf("initiator has at least access level %d", g.cfg.MinReadLevel), g.now().UTC()
	g.audit.write(rec)
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, rt)))
}

// authorizeRead returns 0 when the initiator may read the project, otherwise
// the HTTP status to answer and the reason. Failing lookups refuse.
func (g *Gateway) authorizeRead(ctx context.Context, email, project string) (int, string) {
	if email == "" {
		return http.StatusForbidden, "task has no human initiator"
	}
	user, ok := g.users.get(email)
	if !ok {
		var err error
		if user, err = g.gitlab.findUser(ctx, email); err != nil {
			g.log.Error("git gateway user lookup failed", "error", err)
			return http.StatusServiceUnavailable, "gitlab user lookup failed"
		}
		g.users.put(email, user)
	}
	switch {
	case !user.Found:
		return http.StatusForbidden, "no gitlab user for initiator email"
	case !user.Active:
		return http.StatusForbidden, "gitlab user is not active"
	}
	key := fmt.Sprintf("%d|%s", user.ID, project)
	level, ok := g.levels.get(key)
	if !ok {
		var err error
		if level, err = g.gitlab.accessLevel(ctx, project, user.ID); err != nil {
			g.log.Error("git gateway access lookup failed", "error", err)
			return http.StatusServiceUnavailable, "gitlab access lookup failed"
		}
		g.levels.put(key, level)
	}
	if level < g.cfg.MinReadLevel {
		return http.StatusForbidden, fmt.Sprintf("access level %d is below the %d needed to read", level, g.cfg.MinReadLevel)
	}
	return 0, ""
}

func (g *Gateway) deny(w http.ResponseWriter, r *http.Request, rec AuditRecord, status int) {
	rec.Decision, rec.Time = AuditDeny, g.now().UTC()
	g.audit.write(rec)
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="git gateway"`)
	}
	http.Error(w, "git gateway: "+rec.Reason, status)
}

// bearerOrBasicPassword reads the task token the way git sends it: as the
// password of HTTP Basic auth. A Bearer header is accepted too.
func bearerOrBasicPassword(r *http.Request) string {
	if _, pw, ok := r.BasicAuth(); ok {
		return pw
	}
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
