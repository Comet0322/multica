package sandbox

// The sandbox API is plain HTTP and JSON, so any sandbox provider can be put
// behind it and any client can speak it. These are its bodies, shared by the
// server (package sandboxapi) and the Remote client.
//
//	POST   /v1/sandboxes                  CreateOptions       -> 201 CreatedSandbox
//	POST   /v1/sandboxes/{id}/exec        WireExecRequest     -> 200 WireExecResult
//	PUT    /v1/sandboxes/{id}/files       ?path=&mode=<octal> raw body -> 204
//	GET    /v1/sandboxes/{id}/files       ?path=              -> 200 raw body, 404 if absent
//	DELETE /v1/sandboxes/{id}                                 -> 204
//	GET    /healthz                                           -> 200
//
// Every call but /healthz carries "Authorization: Bearer <token>". Failures are
// {"error": "..."} with a matching status: 401, 404, 413 (too large), 429 (too
// many sandboxes), 400 (bad request).

// CreateOptions asks for a new sandbox. Zero values mean the server's defaults.
type CreateOptions struct {
	Image    string `json:"image,omitempty"`
	MemoryMB int    `json:"memory_mb,omitempty"`
	Pids     int    `json:"pids,omitempty"`
}

// CreatedSandbox answers CreateOptions.
type CreatedSandbox struct {
	ID string `json:"id"`
}

// WireExecRequest is ExecRequest as sent over the API.
type WireExecRequest struct {
	Command    string            `json:"command"`
	Cwd        string            `json:"cwd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`
}

// WireExecResult is ExecResult as sent over the API.
type WireExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	// Truncated is set when the server cut stdout or stderr at its output limit.
	Truncated bool `json:"truncated,omitempty"`
}

// WireError is the body of every failure.
type WireError struct {
	Error string `json:"error"`
}
