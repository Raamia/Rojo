// Package benchmark measures what Rojo actually does on a fixed set of small
// development tasks, so claims about it come from recorded output rather than
// from impression.
//
// The design turns on one distinction. A job reporting `completed` means the
// pipeline's own gate was satisfied; it does not mean the task was done. A
// model can satisfy `go test ./...` by changing nothing, or by writing
// something that compiles and misses the point. So every case carries a hidden
// assertion — a test that is deliberately NOT in the repository the job sees —
// which is applied afterwards and is the only thing this package treats as
// ground truth.
//
// Keeping both numbers is the point. "Pipeline said yes" and "the task was
// actually done" are different measurements, and the gap between them is the
// most honest thing this benchmark produces.
package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Raamia/Rojo/internal/execution"
)

// Case is one benchmark task.
type Case struct {
	Name       string `json:"name"`
	Task       string `json:"task"`
	Difficulty string `json:"difficulty,omitempty"`
	// StartsRed marks a case whose repository fails its own tests before any
	// change is made. Those cases are the ones that exercise the revision loop,
	// and the harness must not treat their initial red as a broken fixture.
	StartsRed bool `json:"starts_red,omitempty"`

	// repoFiles and assertFiles are path -> contents, read from the embedded
	// fixture set. Held in memory because every case is materialised at least
	// twice: once for the job to work in, once clean to apply the patch to.
	repoFiles   map[string][]byte
	assertFiles map[string][]byte
}

// LoadCases reads every case from a fixture tree rooted at dir.
//
// Cases are returned in name order so a run is reproducible: the report is
// meant to be diffed against a previous one, and a map's iteration order would
// reshuffle the rows every time.
func LoadCases(fsys fs.FS, dir string) ([]Case, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read case dir %s: %w", dir, err)
	}

	var cases []Case
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := loadCase(fsys, path(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no cases found under %s", dir)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })
	return cases, nil
}

func loadCase(fsys fs.FS, dir string) (Case, error) {
	meta, err := fs.ReadFile(fsys, path(dir, "case.json"))
	if err != nil {
		return Case{}, fmt.Errorf("read %s/case.json: %w", dir, err)
	}
	var c Case
	if err := json.Unmarshal(meta, &c); err != nil {
		return Case{}, fmt.Errorf("parse %s/case.json: %w", dir, err)
	}
	if strings.TrimSpace(c.Name) == "" {
		return Case{}, fmt.Errorf("%s/case.json has no name", dir)
	}
	if strings.TrimSpace(c.Task) == "" {
		return Case{}, fmt.Errorf("case %s has no task", c.Name)
	}

	if c.repoFiles, err = readTree(fsys, path(dir, "repo")); err != nil {
		return Case{}, err
	}
	if c.assertFiles, err = readTree(fsys, path(dir, "assert")); err != nil {
		return Case{}, err
	}
	if len(c.repoFiles) == 0 {
		return Case{}, fmt.Errorf("case %s has an empty repo/", c.Name)
	}
	// A case with no hidden assertion can only ever report what the pipeline
	// thought of itself, which is the exact self-assessment this package exists
	// to check. Refuse it rather than silently recording a weaker measurement.
	if len(c.assertFiles) == 0 {
		return Case{}, fmt.Errorf("case %s has an empty assert/: there would be no ground truth", c.Name)
	}
	return c, nil
}

// ignoredFixtureFile reports names that are not case material.
//
// The rule mirrors //go:embed's own exclusions — names beginning with "." or
// "_" — so a fixture tree behaves identically whether it is read from an
// embedded FS or from a real directory. Without it a placeholder like .keep
// would count as an assertion file, and a case with no ground truth at all
// would pass the check that exists to prevent exactly that.
func ignoredFixtureFile(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func readTree(fsys fs.FS, root string) (map[string][]byte, error) {
	out := make(map[string][]byte)
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && ignoredFixtureFile(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if ignoredFixtureFile(d.Name()) {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	return out, nil
}

// path joins fs.FS paths, which are always slash-separated regardless of OS.
func path(parts ...string) string { return strings.Join(parts, "/") }

// MaterializeRepo writes the case's starting repository into dir.
func (c Case) MaterializeRepo(dir string) error { return writeTree(dir, c.repoFiles) }

// OverlayAssertions writes the hidden assertion files into dir, on top of
// whatever is already there.
func (c Case) OverlayAssertions(dir string) error { return writeTree(dir, c.assertFiles) }

// embeddedGoMod is the name a fixture's go.mod is stored under.
//
// It cannot be stored as "go.mod": the go tool treats any directory containing
// one as a separate module and excludes it from the parent module's file tree,
// so //go:embed silently skips the whole directory — no error, just a fixture
// with no source files in it. Storing it under a neutral name and restoring the
// real one here is the standard way around that.
const (
	embeddedGoMod = "go.mod.txt"
	realGoMod     = "go.mod"
)

// materializedName maps a stored fixture filename to the name it must have on
// disk for the repository to be a real Go module.
func materializedName(rel string) string {
	if rel == embeddedGoMod || strings.HasSuffix(rel, "/"+embeddedGoMod) {
		return strings.TrimSuffix(rel, embeddedGoMod) + realGoMod
	}
	return rel
}

func writeTree(dir string, files map[string][]byte) error {
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(materializedName(rel)))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create dir for %s: %w", rel, err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}

// InitRepo materialises the case's repository into dir and commits it, so the
// job has a git repository with a clean working tree to cut a worktree from.
//
// The identity is set locally rather than relying on the machine's git config:
// `git commit` refuses without one, and a benchmark that fails on a fresh
// machine for that reason wastes the run.
func (c Case) InitRepo(ctx context.Context, runner execution.CommandRunner, dir string) error {
	if err := c.MaterializeRepo(dir); err != nil {
		return err
	}
	steps := [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "benchmark@rojo.local"},
		{"config", "user.name", "rojo benchmark"},
		{"add", "-A"},
		{"commit", "-m", "benchmark fixture: " + c.Name},
	}
	for _, args := range steps {
		res, err := runner.Run(ctx, dir, "git", args...)
		if err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("git %s exited %d: %s", args[0], res.ExitCode, res.Stderr)
		}
	}
	return nil
}
