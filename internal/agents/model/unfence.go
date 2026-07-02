package model

import "strings"

// Unfence strips a markdown code fence from around a JSON body.
//
// Every agent's system prompt asks for JSON and nothing else, and models
// routinely wrap it in ```json ... ``` anyway. That is a formatting habit, not
// a bad answer, and failing a job over punctuation throws away the work that
// produced it. Anything still not valid JSON after unwrapping is rejected by
// the caller as before.
//
// It lives here, next to LooseString, because this package already owns the
// business of coping with what models actually return. It was previously
// duplicated in the planner and the implementor — and missing from the
// reviewer, which is the worst place to lack it: the reviewer runs last, so a
// fenced reply there discards a completed plan, implementation and
// verification.
func Unfence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:] // drop the opening fence and any language tag
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
