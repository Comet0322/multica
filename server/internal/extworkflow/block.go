package extworkflow

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// BlockLang is the fence info string of a decision block (spec §6.3).
const BlockLang = "ext-workflow"

var (
	ErrBlockMultiple = errors.New("ext-workflow: a comment may carry only one ext-workflow block")
	ErrBlockUnclosed = errors.New("ext-workflow: the ext-workflow block is not closed")
	ErrBlockEmpty    = errors.New("ext-workflow: the ext-workflow block is empty")
	ErrBlockInvalid  = errors.New("ext-workflow: the ext-workflow block is not valid")
)

type blockFields struct {
	Action   string `yaml:"action"`
	Step     string `yaml:"step"`
	To       string `yaml:"to"`
	Reason   string `yaml:"reason"`
	Feedback string `yaml:"feedback"`
}

// ContainsBlock reports whether the comment carries an ext-workflow fence,
// valid or not.
func ContainsBlock(markdown string) bool {
	bodies, unclosed := findBlocks(markdown)
	return len(bodies) > 0 || unclosed
}

// ParseBlock extracts the one decision block of a comment.
func ParseBlock(markdown string) (Decision, bool, error) {
	bodies, unclosed := findBlocks(markdown)
	count := len(bodies)
	if unclosed {
		count++
	}
	switch {
	case count == 0:
		return Decision{}, false, nil
	case count > 1:
		return Decision{}, true, ErrBlockMultiple
	case unclosed:
		return Decision{}, true, ErrBlockUnclosed
	}
	var raw blockFields
	dec := yaml.NewDecoder(strings.NewReader(quoteFreeText(bodies[0])))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return Decision{}, true, ErrBlockEmpty
		}
		return Decision{}, true, fmt.Errorf("%w: %v", ErrBlockInvalid, err)
	}
	d := Decision{
		Action:   DecisionAction(strings.TrimSpace(raw.Action)),
		Step:     strings.TrimSpace(raw.Step),
		To:       strings.TrimSpace(raw.To),
		Reason:   strings.TrimSpace(raw.Reason),
		Feedback: strings.TrimSpace(raw.Feedback),
	}
	if err := ValidateDecision(d); err != nil {
		return d, true, err
	}
	return d, true, nil
}

// freeTextFields are the block fields that hold prose rather than a keyword.
var freeTextFields = []string{"reason", "feedback"}

// quoteFreeText quotes a one-line plain reason or feedback that contains
// ": ". Agents write such prose unquoted, and YAML would read the colon as a
// nested mapping; the whole rest of the line is the value. Keyword fields
// (action, step, to) stay strict.
func quoteFreeText(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		for _, field := range freeTextFields {
			rest, ok := strings.CutPrefix(line, field+":")
			if !ok {
				continue
			}
			value := strings.TrimSpace(rest)
			if !(strings.Contains(value, ": ") || strings.HasSuffix(value, ":")) || strings.ContainsAny(value[:1], `"'|>`) {
				continue
			}
			lines[i] = field + ": " + strconv.Quote(value)
		}
	}
	return strings.Join(lines, "\n")
}

// ValidateDecision checks the fields each action requires (spec §6.3).
func ValidateDecision(d Decision) error {
	need := func(field, value string) error {
		if value == "" {
			return fmt.Errorf("%w: %s needs %s", ErrIllegalDecision, d.Action, field)
		}
		return nil
	}
	switch d.Action {
	case ActionApprove, ActionRetry, ActionSkip:
		return nil
	case ActionRedo:
		return need("feedback", d.Feedback)
	case ActionRewind:
		if err := need("to", d.To); err != nil {
			return err
		}
		return need("feedback", d.Feedback)
	case ActionEscalate, ActionAbort, ActionRequestRewind:
		return need("reason", d.Reason)
	case "":
		return fmt.Errorf("%w: action is required", ErrIllegalDecision)
	}
	return fmt.Errorf("%w: unknown action %q", ErrIllegalDecision, d.Action)
}

type fence struct {
	char byte
	size int
}

// findBlocks returns the bodies of top-level ext-workflow fences (CommonMark
// fences: ``` or ~~~, at most three spaces of indent, closed by the same
// character at least as long). Fences nested inside another fence are text.
func findBlocks(markdown string) (bodies []string, unclosed bool) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	var open *fence
	ours := false
	var buf []string
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if open == nil {
			if indent > 3 {
				continue
			}
			f, info, ok := openingFence(trimmed)
			if !ok {
				continue
			}
			open = &f
			ours = infoLang(info) == BlockLang
			buf = nil
			continue
		}
		if indent <= 3 && closesFence(trimmed, *open) {
			if ours {
				bodies = append(bodies, strings.Join(buf, "\n"))
			}
			open = nil
			continue
		}
		if ours {
			buf = append(buf, line)
		}
	}
	return bodies, open != nil && ours
}

func openingFence(s string) (fence, string, bool) {
	if len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return fence{}, "", false
	}
	n := 0
	for n < len(s) && s[n] == s[0] {
		n++
	}
	if n < 3 {
		return fence{}, "", false
	}
	info := strings.TrimSpace(s[n:])
	if s[0] == '`' && strings.Contains(info, "`") {
		return fence{}, "", false
	}
	return fence{char: s[0], size: n}, info, true
}

func closesFence(s string, f fence) bool {
	n := 0
	for n < len(s) && s[n] == f.char {
		n++
	}
	return n >= f.size && strings.TrimSpace(s[n:]) == ""
}

func infoLang(info string) string {
	if i := strings.IndexAny(info, " \t{"); i >= 0 {
		info = info[:i]
	}
	return strings.ToLower(info)
}
