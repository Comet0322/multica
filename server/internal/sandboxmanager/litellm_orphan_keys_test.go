package sandboxmanager_test

import (
	"testing"
	"time"
)

// Task keys live in LiteLLM, the leases only in the manager's memory. After a
// manager restart the keys of the tasks that were running are still valid
// (budget and rate caps, but valid until they expire hours later) and nothing
// would revoke them. At the first registration, when no task of this manager
// can hold a key yet, the manager deletes the task keys of its workspace that
// an earlier manager issued, and nothing else.
func TestFirstRegistrationRevokesTaskKeysLeftByAnEarlierManager(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com")
	llm.seedKey("orphan-task", harnessWorkspaceID, "orphan-task")
	llm.seedKey("other-ws-task", "another-workspace", "other-ws-task")
	llm.seedKey("alice-personal-key", "", "")
	h := newHarness(t, 1, withLiteLLM(llm))

	h.newPod("127.0.0.2", "pool-0").start()

	eventually(t, "the earlier manager's task key is deleted", func() bool { return llm.aliasDeleted("orphan-task") })
	time.Sleep(100 * time.Millisecond)
	if llm.aliasDeleted("other-ws-task") {
		t.Fatal("a task key of another workspace was deleted")
	}
	if llm.aliasDeleted("alice-personal-key") {
		t.Fatal("a key without task metadata was deleted")
	}
}
