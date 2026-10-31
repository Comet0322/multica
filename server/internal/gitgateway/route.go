package gitgateway

import (
	"net/http"
	"net/url"
	"strings"
)

// Git read actions the gateway lets through.
const (
	ActionInfoRefs   = "git.info_refs"
	ActionUploadPack = "git.upload_pack"
)

// Git write actions the gateway lets through, subject to the push ref policy.
const (
	ActionPushInfoRefs = "git.push_info_refs"
	ActionReceivePack  = "git.receive_pack"
)

// route is a request that passed the allow list, reduced to values the
// gateway itself builds. Authorization and forwarding both use these, never
// the client's raw path, so what is checked is what is sent.
type route struct {
	action  string
	project string // lowercase, no ".git" suffix
	path    string // path to request from GitLab
	query   string
	write   bool // a push; authorized by servePush, not authorizeRead
}

// parseRoute normalizes the request and matches it against the allow list. It
// returns a reason when the request must be refused. Only these pass:
//
//	GET  /<namespace>/<project>.git/info/refs?service=git-upload-pack
//	POST /<namespace>/<project>.git/git-upload-pack
//	GET  /<namespace>/<project>.git/info/refs?service=git-receive-pack
//	POST /<namespace>/<project>.git/git-receive-pack
func parseRoute(method string, u *url.URL) (route, string) {
	project, tail, why := normalizeProjectPath(u.EscapedPath())
	if why != "" {
		return route{}, why
	}
	var rt route
	switch tail {
	case "info/refs":
		if method != http.MethodGet {
			return route{}, "method not allowed"
		}
		switch u.RawQuery {
		case "service=git-upload-pack":
			rt = route{action: ActionInfoRefs, query: u.RawQuery}
		case "service=git-receive-pack":
			rt = route{action: ActionPushInfoRefs, query: u.RawQuery, write: true}
		default:
			return route{}, "service not allowed"
		}
	case "git-upload-pack":
		if method != http.MethodPost {
			return route{}, "method not allowed"
		}
		if u.RawQuery != "" {
			return route{}, "query not allowed"
		}
		rt = route{action: ActionUploadPack}
	case "git-receive-pack":
		if method != http.MethodPost {
			return route{}, "method not allowed"
		}
		if u.RawQuery != "" {
			return route{}, "query not allowed"
		}
		rt = route{action: ActionReceivePack, write: true}
	default:
		return route{}, "path not allowed"
	}
	rt.project = project
	rt.path = "/" + project + ".git/" + tail
	return rt, ""
}

// normalizeProjectPath splits an escaped URL path into the project path and
// the trailing git endpoint. It decodes once, collapses repeated slashes,
// lowercases (GitLab paths are case-insensitive), and refuses dot segments,
// double encoding, backslashes and control characters rather than guessing
// what they were meant to be.
func normalizeProjectPath(escaped string) (project, tail, why string) {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return "", "", "invalid path encoding"
	}
	if strings.ContainsRune(decoded, '%') {
		return "", "", "double-encoded path"
	}
	for _, r := range decoded {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", "", "invalid character in path"
		}
	}
	var segs []string
	for _, s := range strings.Split(strings.ToLower(decoded), "/") {
		switch s {
		case "":
		case ".", "..":
			return "", "", "dot segments not allowed"
		default:
			segs = append(segs, s)
		}
	}
	// The endpoint is the last one or two segments.
	var tailSegs int
	switch {
	case len(segs) >= 1 && (segs[len(segs)-1] == "git-upload-pack" || segs[len(segs)-1] == "git-receive-pack"):
		tailSegs = 1
	case len(segs) >= 2 && segs[len(segs)-2] == "info" && segs[len(segs)-1] == "refs":
		tailSegs = 2
	default:
		return "", "", "path not allowed"
	}
	tail = strings.Join(segs[len(segs)-tailSegs:], "/")
	segs = segs[:len(segs)-tailSegs]
	if len(segs) < 2 {
		return "", "", "path not allowed"
	}
	last := &segs[len(segs)-1]
	if !strings.HasSuffix(*last, ".git") {
		return "", "", "path not allowed"
	}
	*last = strings.TrimSuffix(*last, ".git")
	for _, s := range segs {
		if !validSegment(s) {
			return "", "", "invalid project path"
		}
	}
	return strings.Join(segs, "/"), tail, ""
}

// validSegment accepts the characters GitLab allows in group and project paths.
func validSegment(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		case (r == '-' || r == '.') && i > 0:
		default:
			return false
		}
	}
	return true
}
