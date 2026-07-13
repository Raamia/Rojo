// Package repocontext selects the slice of a repository worth showing a model.
//
// A planner given only a task and a path plans blind. It needs to know what
// files exist and roughly where the task's subject lives. This deliberately
// uses git rather than embeddings: `git ls-files` and `git grep` are exact,
// instant, need no index to build or keep fresh, and only ever see files the
// repository actually tracks. Measuring whether retrieval improves task success
// comes before reaching for anything heavier.
package repocontext

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/workspace"
)

// Defaults bound how much context is gathered. The cost of being wrong is
// paid in tokens on every planning call, so the caps are deliberately modest.
const (
	DefaultMaxFiles    = 40
	DefaultMaxKeywords = 12
)

// manifests are always worth including: they name the language, the module,
// and the dependencies, which is most of what orients a reader in a new repo.
var manifests = map[string]bool{
	"go.mod": true, "package.json": true, "Cargo.toml": true,
	"pyproject.toml": true, "requirements.txt": true, "Gemfile": true,
	"pom.xml": true, "build.gradle": true, "composer.json": true,
	"Makefile": true, "README.md": true,
}

// stopwords are the words a task description is mostly made of. Grepping for
// "the" or "add" matches everything, which is the same as matching nothing.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"if": true, "then": true, "for": true, "to": true, "of": true, "in": true,
	"on": true, "at": true, "by": true, "with": true, "from": true, "into": true,
	"is": true, "are": true, "was": true, "be": true, "been": true, "it": true,
	"this": true, "that": true, "these": true, "those": true, "as": true,
	"add": true, "adds": true, "make": true, "makes": true, "fix": true,
	"fixes": true, "update": true, "updates": true, "change": true, "new": true,
	"use": true, "using": true, "should": true, "when": true, "where": true,
	"all": true, "any": true, "not": true, "no": true, "so": true, "we": true,
	"i": true, "you": true, "it's": true, "its": true, "can": true, "will": true,
	// Politeness and framing words are common in real task descriptions and
	// carry no signal about which files matter.
	"please": true, "could": true, "would": true, "want": true, "need": true,
	"let": true, "lets": true, "just": true, "also": true,
	"there": true, "here": true, "some": true, "more": true, "than": true,
	"have": true, "has": true, "had": true, "get": true, "gets": true,
	"do": true, "does": true, "did": true, "done": true, "now": true,
	"remove": true, "removes": true, "create": true, "creates": true,
	"implement": true, "implements": true, "support": true, "handle": true,
	// Generic programming vocabulary. In a code repository these match almost
	// every file, which makes them worth exactly as much as "the" — and worse
	// than useless, because they crowd real signal out of the keyword budget.
	// A live run lost "main" to this: "add a Greet function that takes a name
	// string and returns a greeting, and call it from main" spent all eight
	// slots on function/takes/string/returns/call and never searched for the
	// one word that would have found cmd/tutorial/main.go.
	"function": true, "functions": true, "func": true,
	"method": true, "methods": true, "parameter": true, "parameters": true,
	"argument": true, "arguments": true, "takes": true, "taking": true,
	"return": true, "returns": true, "returning": true,
	"call": true, "calls": true, "calling": true, "called": true,
	"string": true, "strings": true, "bool": true, "boolean": true,
	"code": true, "line": true, "lines": true,
}

// DefaultMaxTreePaths caps the file list handed to a model. Paths are cheap —
// a few hundred cost a couple of thousand tokens — and knowing the layout is
// what stops a model inventing a plausible-but-wrong location for its change.
const DefaultMaxTreePaths = 300

// Context is what gets handed to the planner.
type Context struct {
	// Files are repository-relative paths, most relevant first.
	Files []string
	// Tree is every path the repository tracks, capped at DefaultMaxTreePaths.
	//
	// Keyword ranking decides which files are worth sending in full; this is
	// the map that says what exists at all. Without it a model asked to "call
	// it from main" cannot tell whether main lives at ./main.go or
	// ./cmd/tutorial/main.go, and a confident guess produces a change in the
	// wrong place that still compiles — which the gate cannot catch and a
	// reviewer, seeing only the diff, approves. Observed on the first live run.
	Tree []string
	// Keywords are the terms extracted from the task and searched for. Returned
	// so a caller can see why these files were chosen.
	Keywords []string
	// TotalTracked is how many files the repository tracks, so the planner can
	// tell a 40-of-45 selection from a 40-of-40000 one.
	TotalTracked int
}

type Selector struct {
	runner execution.CommandRunner
	// MaxFiles caps how many paths are returned. Zero uses DefaultMaxFiles.
	MaxFiles int
}

func NewSelector(runner execution.CommandRunner) *Selector {
	return &Selector{runner: runner}
}

func (s *Selector) maxFiles() int {
	if s.MaxFiles <= 0 {
		return DefaultMaxFiles
	}
	return s.MaxFiles
}

// Select gathers context for a task.
//
// It never fails the caller: a repository that is empty, has no matches, or
// whose git commands error still yields a usable (possibly empty) Context. A
// planner with thin context should produce a worse plan, not no plan — and the
// verification gate is what catches a bad one either way.
func (s *Selector) Select(ctx context.Context, repoPath, task string) (Context, error) {
	tracked, err := s.trackedFiles(ctx, repoPath)
	if err != nil {
		return Context{}, fmt.Errorf("list tracked files: %w", err)
	}

	out := Context{Keywords: Keywords(task), TotalTracked: len(tracked)}
	out.Tree = tracked
	if len(out.Tree) > DefaultMaxTreePaths {
		out.Tree = out.Tree[:DefaultMaxTreePaths]
	}
	if len(tracked) == 0 {
		return out, nil
	}

	// Rank by how many distinct signals point at a file. A file that both
	// matches a task keyword and is a manifest outranks one that only does one.
	score := make(map[string]int, len(tracked))
	trackedSet := make(map[string]bool, len(tracked))
	for _, f := range tracked {
		trackedSet[f] = true
	}

	for _, kw := range out.Keywords {
		// A keyword appearing in a path is a strong signal — a task about
		// "ratelimit" almost certainly concerns ratelimit.go.
		for _, f := range tracked {
			if strings.Contains(strings.ToLower(f), kw) {
				score[f] += 3
			}
		}
		for _, f := range s.grepFiles(ctx, repoPath, kw) {
			if trackedSet[f] {
				score[f] += 2
			}
		}
	}

	for _, f := range tracked {
		if manifests[path.Base(f)] {
			score[f] += 1
		}
	}

	ranked := make([]string, 0, len(score))
	for f, n := range score {
		if n > 0 {
			ranked = append(ranked, f)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if score[ranked[i]] != score[ranked[j]] {
			return score[ranked[i]] > score[ranked[j]]
		}
		return ranked[i] < ranked[j] // stable, so the same repo yields the same context
	})

	if len(ranked) > s.maxFiles() {
		ranked = ranked[:s.maxFiles()]
	}
	out.Files = ranked
	return out, nil
}

// trackedFiles lists what the repository tracks. Using git rather than walking
// the filesystem means .gitignore is respected for free — no vendor/,
// node_modules/, or build output.
func (s *Selector) trackedFiles(ctx context.Context, repoPath string) ([]string, error) {
	res, err := s.runner.Run(ctx, repoPath, "git", workspace.SafeGitArgs("ls-files")...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("git ls-files exited %d: %s", res.ExitCode, res.Stderr)
	}
	return splitLines(res.Stdout), nil
}

// grepFiles returns the tracked files containing a keyword.
//
// A failure here is not fatal: grep exits 1 when nothing matched, which is an
// ordinary outcome, and any other failure just means this keyword contributes
// no signal.
func (s *Selector) grepFiles(ctx context.Context, repoPath, keyword string) []string {
	res, err := s.runner.Run(ctx, repoPath, "git",
		workspace.SafeGitArgs("grep", "--ignore-case", "--fixed-strings", "--name-only", keyword)...)
	if err != nil || res.ExitCode != 0 {
		return nil
	}
	return splitLines(res.Stdout)
}

// Keywords extracts the searchable terms from a task description.
//
// Exported because what the planner searched for is worth asserting on, and
// worth showing a user who wonders why a given file was selected.
func Keywords(task string) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range strings.FieldsFunc(strings.ToLower(task), func(r rune) bool {
		return !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		// Two-character tokens match far too much to be worth a search.
		if len(raw) < 3 || stopwords[raw] || seen[raw] {
			continue
		}
		seen[raw] = true
		out = append(out, raw)
		if len(out) == DefaultMaxKeywords {
			break
		}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
