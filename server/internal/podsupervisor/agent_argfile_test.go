//go:build linux

package podsupervisor_test

import (
	"os"
	"strings"
	"testing"
)

// The daemon writes the MCP configuration of a task (it carries the gateway
// ticket) to a 0600 file in its own TMPDIR, which is inside its private home,
// and hands the path to the agent CLI as --mcp-config. The agent container does
// not have that volume, so Claude Code failed at start. The shim reads the file
// and the runner gives the agent a copy in the shared TMPDIR, passing that
// path instead.
const argFileDaemon = `#!/bin/sh
echo $$ >> "$OUT/pids"
if [ ! -e "$OUT/ran" ]; then
  : > "$OUT/ran"
  mkdir -m 700 "$TMPDIR/multica-mcp-1"
  printf '{"mcpServers":{}}' > "$TMPDIR/multica-mcp-1/mcp-config.json"
  chmod 600 "$TMPDIR/multica-mcp-1/mcp-config.json"
  echo "$TMPDIR/multica-mcp-1/mcp-config.json" > "$OUT/daemon.cfg"
  # The agent's environment need not carry the daemon's TMPDIR.
  TMPDIR=/nonexistent fakeagent --strict-mcp-config --mcp-config "$TMPDIR/multica-mcp-1/mcp-config.json" --other=value > "$OUT/agent.out" 2>&1
  echo $? > "$OUT/agent.status"
fi
exec sleep 100000
`

const argFileAgent = `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
  --mcp-config) shift; echo "path:$1"; echo "content:$(cat "$1")" ;;
  --other=*) echo "other:$1" ;;
  esac
  shift
done
`

func TestAgentGetsAReadableCopyOfTheDaemonsConfigFile(t *testing.T) {
	p := startSplitPod(t, argFileDaemon, argFileAgent)
	p.waitOutFile("agent.status")
	out := p.outFile("agent.out") + "\n"

	if !strings.Contains(out, `content:{"mcpServers":{}}`+"\n") {
		t.Fatalf("the agent could not read the file it was given:\n%s", out)
	}
	if !strings.Contains(out, "other:--other=value\n") {
		t.Errorf("other arguments must pass unchanged:\n%s", out)
	}
	var path string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "path:"); ok {
			path = v
		}
	}
	if path == "" || path == p.outFile("daemon.cfg") || strings.HasPrefix(path, p.daemonHome+"/") {
		t.Errorf("the agent was handed %q; want a copy outside the daemon's home (original %q)", path, p.outFile("daemon.cfg"))
	}
	if !strings.HasPrefix(path, p.tmp+"/") {
		t.Errorf("copy %q is not in the agent's TMPDIR %q", path, p.tmp)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("the copy %q outlives the agent", path)
	}
}
