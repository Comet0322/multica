// server/internal/handler/comment_note_workflow_test.go
package handler

import "testing"

func TestWorkflowCommandsSuppressAgentTriggering(t *testing.T) {
	for _, c := range []string{"/accept", "/REJECT needs work", "  /retry"} {
		if !isNoteComment(c) {
			t.Errorf("isNoteComment(%q) = false, want true so agents are not woken", c)
		}
	}
	for _, c := range []string{"/accepted", "please /accept", "/notes", "hello"} {
		if isNoteComment(c) {
			t.Errorf("isNoteComment(%q) = true, want false", c)
		}
	}
	if !isNoteComment("/note hi") {
		t.Error("existing /note behaviour must be unchanged")
	}
}
