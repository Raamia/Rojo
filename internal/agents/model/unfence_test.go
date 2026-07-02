package model

import "testing"

func TestUnfence(t *testing.T) {
	body := `{"decision":"approve"}`
	tests := map[string]string{
		"bare":              body,
		"fenced":            "```\n" + body + "\n```",
		"tagged":            "```json\n" + body + "\n```",
		"surrounding space": "  ```json\n" + body + "\n```  ",
		"no trailing fence": "```json\n" + body,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if got := Unfence(input); got != body {
				t.Errorf("Unfence(%q) = %q, want %q", input, got, body)
			}
		})
	}
}

// A body that merely contains backticks is not fenced, and must survive intact.
func TestUnfence_LeavesUnfencedTextAlone(t *testing.T) {
	for _, s := range []string{
		`{"notes":"use ` + "`go test`" + ` here"}`,
		"plain prose, no JSON at all",
		"",
	} {
		if got := Unfence(s); got != trimSpace(s) {
			t.Errorf("Unfence(%q) = %q, want it unchanged", s, got)
		}
	}
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
