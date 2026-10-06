package extworkflow

import (
	"errors"
	"testing"
)

func TestParseBlock(t *testing.T) {
	cases := []struct {
		name      string
		markdown  string
		wantFound bool
		wantErr   error
		want      Decision
	}{
		{
			name:     "no block",
			markdown: "Looks good to me.\n\n```go\nfmt.Println(1)\n```",
		},
		{
			name:      "rewind with every field",
			markdown:  "Rewinding.\n\n```ext-workflow\naction: rewind          # approve | redo | ...\nstep: backend\nto: spec\nreason: Spec assumed paginated API; it is not.\nfeedback: |\n  Switch to cursor pagination and update the data-flow section.\n```\n",
			wantFound: true,
			want: Decision{Action: ActionRewind, Step: "backend", To: "spec",
				Reason: "Spec assumed paginated API; it is not.", Feedback: "Switch to cursor pagination and update the data-flow section."},
		},
		{
			name:      "approve needs nothing else",
			markdown:  "```ext-workflow\naction: approve\n```",
			wantFound: true,
			want:      Decision{Action: ActionApprove},
		},
		{
			name:      "tilde fence and info words",
			markdown:  "~~~ext-workflow decision\naction: skip\n~~~",
			wantFound: true,
			want:      Decision{Action: ActionSkip},
		},
		{
			name:      "request-rewind without target",
			markdown:  "```ext-workflow\naction: request-rewind\nreason: upstream output is wrong\n```",
			wantFound: true,
			want:      Decision{Action: ActionRequestRewind, Reason: "upstream output is wrong"},
		},
		{
			name:      "unicode values survive",
			markdown:  "```ext-workflow\naction: redo\nfeedback: 改用游标分页 ✅\n```",
			wantFound: true,
			want:      Decision{Action: ActionRedo, Feedback: "改用游标分页 ✅"},
		},
		{
			name:      "unicode reason for escalate",
			markdown:  "```ext-workflow\naction: escalate\nreason: 规格假设了分页 API；实际上没有。\n```",
			wantFound: true,
			want:      Decision{Action: ActionEscalate, Reason: "规格假设了分页 API；实际上没有。"},
		},
		{
			name:      "unknown field is rejected",
			markdown:  "```ext-workflow\naction: approve\nnote: hi\n```",
			wantFound: true,
			wantErr:   ErrBlockInvalid,
		},
		{
			name:      "two blocks are rejected",
			markdown:  "```ext-workflow\naction: approve\n```\n\n```ext-workflow\naction: skip\n```",
			wantFound: true,
			wantErr:   ErrBlockMultiple,
		},
		{
			name:      "unclosed block is rejected",
			markdown:  "```ext-workflow\naction: approve\n",
			wantFound: true,
			wantErr:   ErrBlockUnclosed,
		},
		{
			name:      "empty block is rejected",
			markdown:  "```ext-workflow\n```",
			wantFound: true,
			wantErr:   ErrBlockEmpty,
		},
		{
			name:      "missing action",
			markdown:  "```ext-workflow\nreason: x\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "unknown action",
			markdown:  "```ext-workflow\naction: promote\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "redo needs feedback",
			markdown:  "```ext-workflow\naction: redo\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "rewind needs to",
			markdown:  "```ext-workflow\naction: rewind\nfeedback: x\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "abort needs a reason",
			markdown:  "```ext-workflow\naction: abort\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "request-rewind needs a reason",
			markdown:  "```ext-workflow\naction: request-rewind\nto: spec\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:     "a fenced example inside a longer fence is not a block",
			markdown: "````markdown\n```ext-workflow\naction: approve\n```\n````",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := ParseBlock(tc.markdown)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBlock: %v", err)
			}
			if got != tc.want {
				t.Fatalf("decision = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestContainsBlock(t *testing.T) {
	for markdown, want := range map[string]bool{
		"plain text":                                   false,
		"```go\nx := 1\n```":                           false,
		"```ext-workflow\naction: approve\n```":        true,
		"```ext-workflow\nthis: is not yaml: at all\n": true, // unclosed but still a decision attempt
		"```EXT-WORKFLOW\naction: approve\n```":        true,
	} {
		if got := ContainsBlock(markdown); got != want {
			t.Fatalf("ContainsBlock(%q) = %v, want %v", markdown, got, want)
		}
	}
}
