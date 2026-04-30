package jobs

import (
	"errors"
	"strings"
	"testing"
)

// These tests document the ACTUAL behavior of NewJobRequest.Validate against the
// request-validation contract ("minTaskLength = 4, maxTaskLength = 4000",
// "Task is strings.TrimSpace'd before length check", "RepoPath: non-empty after
// trim, and filepath.IsAbs returns true").
//
// Test names starting with "BUG_" mark places where actual behavior diverges from
// what the documented contract implies. They still assert the real behavior so the
// suite passes; they exist to pin the bug down until it is fixed.

const okRepo = "/tmp/repo"

func validate(task, repo string) error {
	// Validate normalizes in place, so it needs an addressable request.
	req := NewJobRequest{Task: task, RepoPath: repo}
	return req.Validate()
}

// ---------------------------------------------------------------------------
// 1. Task length boundaries (min 4, max 4000)
// ---------------------------------------------------------------------------

func TestRobustness_TaskLengthBoundariesAreExact(t *testing.T) {
	cases := []struct {
		name    string
		n       int
		wantErr error
	}{
		{"3 chars is below the minimum", 3, ErrTaskTooShort},
		{"4 chars is exactly the minimum", 4, nil},
		{"5 chars is above the minimum", 5, nil},
		{"3999 chars is under the maximum", 3999, nil},
		{"4000 chars is exactly the maximum", 4000, nil},
		{"4001 chars is over the maximum", 4001, ErrTaskTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(strings.Repeat("a", tc.n), okRepo)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("len=%d: got err %v, want %v", tc.n, err, tc.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. BUG: length is measured in BYTES, not runes/characters
// ---------------------------------------------------------------------------

// The API describes a 4000-character limit. The implementation uses len(string),
// which counts BYTES. A task of 4000 emoji is 4000 characters but 16000 bytes and is
// rejected. Non-ASCII users get a limit 4x smaller than documented.
func TestRobustness_TaskMaxLengthIsMeasuredInRunes(t *testing.T) {
	task := strings.Repeat("\U0001F600", 4000) // 4000 runes, 16000 bytes
	if got, want := len([]rune(task)), 4000; got != want {
		t.Fatalf("precondition: got %d runes, want %d", got, want)
	}
	if got, want := len(task), 16000; got != want {
		t.Fatalf("precondition: got %d bytes, want %d", got, want)
	}

	// FIXED: the limit counts characters, so 4000 emoji are 4000 characters.
	if err := validate(task, okRepo); err != nil {
		t.Fatalf("a 4000-character task should be accepted, got %v", err)
	}
	if err := validate(strings.Repeat("\U0001F600", 4001), okRepo); !errors.Is(err, ErrTaskTooLong) {
		t.Fatalf("4001 characters should be too long, got %v", err)
	}
}

// The same byte/rune confusion at the low end: minTaskLength=4 is meant to reject
// trivially short tasks, but ONE emoji is 4 bytes, so a single-character task passes.
func TestRobustness_MinimumTaskLengthIsMeasuredInRunes(t *testing.T) {
	// FIXED: one emoji is one character, so it no longer satisfies a
	// 4-character minimum just by being 4 bytes wide.
	if err := validate("\U0001F600", okRepo); !errors.Is(err, ErrTaskTooShort) {
		t.Fatalf("a 1-character task should be too short, got %v", err)
	}
	// The minimum is now consistent across scripts: 3 characters is 3
	// characters whether ASCII or CJK.
	if err := validate("\u4f60\u597d\u5417", okRepo); !errors.Is(err, ErrTaskTooShort) {
		t.Fatalf("a 3-character CJK task should be too short, got %v", err)
	}
	if err := validate("abc", okRepo); !errors.Is(err, ErrTaskTooShort) {
		t.Fatalf("a 3-character ASCII task should be too short, got %v", err)
	}
	if err := validate("abcd", okRepo); err != nil {
		t.Fatalf("a 4-character task should pass, got %v", err)
	}
}

func TestRobustness_WhitespacePaddingCannotBypassMaxTaskLength(t *testing.T) {
	// 1 MiB of spaces on each side of a 4-character task.
	pad := strings.Repeat(" ", 1<<20)
	req := NewJobRequest{Task: pad + "abcd" + pad, RepoPath: okRepo}

	// FIXED: Validate normalizes in place, so the padding is stripped before
	// the length check AND before the caller stores the value. Previously the
	// check ran on the trimmed value while the handler persisted the raw one,
	// so a 2 MiB task passed a 4000-character limit.
	if err := req.Validate(); err != nil {
		t.Fatalf("got err %v, want nil", err)
	}
	if req.Task != "abcd" {
		t.Errorf("stored task is %d bytes, want the trimmed 4-character value", len(req.Task))
	}
}

// ---------------------------------------------------------------------------
// 4. Whitespace and unicode tasks
// ---------------------------------------------------------------------------

func TestRobustness_WhitespaceOnlyTasksAreRejected(t *testing.T) {
	cases := map[string]string{
		"spaces":                   "     ",
		"tabs":                     "\t\t\t\t\t",
		"newlines":                 "\n\n\n\n",
		"crlf":                     "\r\n\r\n\r\n",
		"vertical tab + formfeed":  "\v\f\v\f",
		"unicode NBSP U+00A0":      "\u00a0\u00a0\u00a0",
		"ideographic space U+3000": "\u3000\u3000\u3000",
		"NEL U+0085":               "\u0085\u0085\u0085",
		"mixed":                    " \t\n\r\v\f\u00a0\u3000",
	}
	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validate(task, okRepo); !errors.Is(err, ErrTaskRequired) {
				t.Fatalf("got err %v, want ErrTaskRequired", err)
			}
		})
	}
}

// strings.TrimSpace uses unicode.IsSpace, which does NOT include zero-width or
// formatting characters. A task made only of invisible characters is accepted.
func TestRobustness_BUG_ZeroWidthAndFormattingCharsCountAsAValidTask(t *testing.T) {
	cases := map[string]string{
		"zero-width space U+200B":      strings.Repeat("\u200b", 4),
		"zero-width non-joiner U+200C": strings.Repeat("\u200c", 4),
		"word joiner U+2060":           strings.Repeat("\u2060", 4),
		"soft hyphen U+00AD":           strings.Repeat("\u00ad", 4),
		"BOM as content U+FEFF":        strings.Repeat("\ufeff", 4),
		"RTL override U+202E":          strings.Repeat("\u202e", 4),
	}
	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validate(task, okRepo); err != nil {
				t.Fatalf("got err %v, want nil (documenting actual behavior)", err)
			}
		})
	}
	t.Log("CONFIRMED BUG: a visually empty task of zero-width characters is accepted " +
		"as a valid job description")
}

// A right-to-left override embedded in an otherwise normal task survives validation
// and will be rendered by any log viewer / UI that honors bidi control characters.
func TestRobustness_RTLOverrideInTaskIsAcceptedUnsanitized(t *testing.T) {
	task := "delete \u202egnitset/ tmp/"
	if err := validate(task, okRepo); err != nil {
		t.Fatalf("got err %v, want nil", err)
	}
	t.Log("NOTE: bidi control characters are not stripped; task text is stored verbatim")
}

func TestRobustness_TaskWithEmbeddedNULByteIsRejected(t *testing.T) {
	// A JSON body may legally carry \u0000, which decodes to a real NUL byte.
	// FIXED: rejected as a 400 here rather than failing later as a 500 on a
	// postgres text column (invalid byte sequence for encoding UTF8: 0x00).
	if err := validate("ab\x00cd", okRepo); !errors.Is(err, ErrNullByte) {
		t.Fatalf("got err %v, want ErrNullByte", err)
	}
	if err := validate("a valid task", "/tmp/re\x00po"); !errors.Is(err, ErrNullByte) {
		t.Fatalf("repo_path NUL: got err %v, want ErrNullByte", err)
	}
}

// ---------------------------------------------------------------------------
// 5. repo_path edge cases
// ---------------------------------------------------------------------------

func TestRobustness_RepoPathValidationMatchesDocumentedRules(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr error
	}{
		// Accepted: filepath.IsAbs is the only structural rule.
		{"filesystem root", "/", nil},
		{"double slash root", "//", nil},
		{"triple slash", "///", nil},
		{"normal path", "/tmp/repo", nil},
		{"traversal above root", "/../etc", nil},
		{"traversal in the middle", "/tmp/../../etc/passwd", nil},
		{"trailing slash", "/tmp/repo/", nil},
		{"dot segments", "/tmp/./repo/./", nil},
		{"trailing space kept", "/tmp/repo ", nil},
		{"embedded NUL byte", "/tmp/re\x00po", ErrNullByte},
		{"embedded newline", "/tmp/re\npo", nil},
		{"looks like a flag", "/--upload-pack=evil", nil},
		{"windows path with forward slash", "/C:/windows", nil},

		// Rejected.
		{"empty", "", ErrRepoPathMissing},
		{"whitespace only", "   \t\n ", ErrRepoPathMissing},
		{"relative", "repo", ErrRepoPathRelative},
		{"dot relative", "./repo", ErrRepoPathRelative},
		{"parent relative", "../repo", ErrRepoPathRelative},
		{"tilde home", "~/repo", ErrRepoPathRelative},
		{"windows drive path on unix", `C:\windows\system32`, ErrRepoPathRelative},
		{"windows UNC path on unix", `\\server\share`, ErrRepoPathRelative},
		{"file url", "file:///tmp/repo", ErrRepoPathRelative},
		{"leading space before absolute", " /tmp/repo", nil}, // trimmed before the IsAbs check
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate("a valid task", tc.path)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("repo_path=%q: got err %v, want %v", tc.path, err, tc.wantErr)
			}
		})
	}
}

// Validate() trims repo_path for the emptiness check but then calls filepath.IsAbs
// on the UNTRIMMED value. " /tmp/repo" is therefore reported as "must be an absolute
// path" -- a misleading error for a path that is absolute once trimmed.
func TestRobustness_LeadingWhitespaceRepoPathIsTrimmedNotMisreported(t *testing.T) {
	// FIXED: repo_path is trimmed once, up front, so it is no longer trimmed
	// for the emptiness check but left padded for the IsAbs check.
	if err := validate("a valid task", " /tmp/repo"); err != nil {
		t.Fatalf("got err %v, want nil for a path that is absolute once trimmed", err)
	}
	req := NewJobRequest{Task: "a valid task", RepoPath: "/tmp/repo "}
	if err := req.Validate(); err != nil {
		t.Fatalf("trailing space: got %v", err)
	}
	if req.RepoPath != "/tmp/repo" {
		t.Errorf("repo_path = %q, want the trimmed value to be stored", req.RepoPath)
	}
}

// There is no upper bound on repo_path at all.
func TestRobustness_RepoPathHasAMaximumLength(t *testing.T) {
	// FIXED: repo_path is capped. PATH_MAX is 4096, so anything longer cannot
	// name a real path and should not be persisted.
	for _, n := range []int{65536, 1 << 20} {
		path := "/" + strings.Repeat("a", n)
		if err := validate("a valid task", path); !errors.Is(err, ErrRepoPathTooLong) {
			t.Fatalf("len=%d: got err %v, want ErrRepoPathTooLong", n, err)
		}
	}
	if err := validate("a valid task", "/"+strings.Repeat("a", 100)); err != nil {
		t.Fatalf("an ordinary path should pass, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 6. Validation ordering
// ---------------------------------------------------------------------------

func TestRobustness_TaskIsValidatedBeforeRepoPath(t *testing.T) {
	// Both fields are invalid; the task error wins. Clients cannot see all
	// validation problems in one round trip.
	err := validate("", "relative/path")
	if !errors.Is(err, ErrTaskRequired) {
		t.Fatalf("got err %v, want ErrTaskRequired", err)
	}
	t.Log("NOTE: Validate() returns the first error only, not a field-keyed error set")
}
