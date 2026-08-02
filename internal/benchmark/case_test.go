package benchmark

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// fakeFixtures builds an in-memory case tree, so loading is tested without
// depending on the real fixtures — those get their own integrity test where
// they are embedded.
func fakeFixtures() fstest.MapFS {
	return fstest.MapFS{
		"cases/alpha/case.json": &fstest.MapFile{
			Data: []byte(`{"name":"alpha","task":"do the thing","difficulty":"easy"}`),
		},
		"cases/alpha/repo/go.mod.txt":  &fstest.MapFile{Data: []byte("module benchcase\n\ngo 1.25\n")},
		"cases/alpha/repo/a.go":        &fstest.MapFile{Data: []byte("package benchcase\n")},
		"cases/alpha/repo/sub/b.go":    &fstest.MapFile{Data: []byte("package sub\n")},
		"cases/alpha/assert/a_test.go": &fstest.MapFile{Data: []byte("package benchcase\n")},
		"cases/beta/case.json":         &fstest.MapFile{Data: []byte(`{"name":"beta","task":"another"}`)},
		"cases/beta/repo/go.mod.txt":   &fstest.MapFile{Data: []byte("module benchcase\n")},
		"cases/beta/assert/b_test.go":  &fstest.MapFile{Data: []byte("package benchcase\n")},
	}
}

func TestLoadCases(t *testing.T) {
	cases, err := LoadCases(fakeFixtures(), "cases")
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("got %d cases, want 2", len(cases))
	}
	// Sorted, so a report can be diffed against a previous run.
	if cases[0].Name != "alpha" || cases[1].Name != "beta" {
		t.Errorf("cases = %v, want [alpha beta] in name order", names(cases))
	}
	if cases[0].Task != "do the thing" || cases[0].Difficulty != "easy" {
		t.Errorf("metadata not parsed: %+v", cases[0])
	}
}

// The stored go.mod.txt must land on disk as a real go.mod, or the fixture is
// not a Go module and every check against it fails for the wrong reason.
func TestMaterializeRepo_RestoresGoMod(t *testing.T) {
	cases, err := LoadCases(fakeFixtures(), "cases")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := cases[0].MaterializeRepo(dir); err != nil {
		t.Fatalf("MaterializeRepo: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatalf("go.mod was not restored: %v", err)
	}
	if !strings.Contains(string(b), "module benchcase") {
		t.Errorf("go.mod = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod.txt")); err == nil {
		t.Error("go.mod.txt was written verbatim; the stored name must not survive")
	}
	// Nested directories have to survive too.
	if _, err := os.Stat(filepath.Join(dir, "sub", "b.go")); err != nil {
		t.Errorf("nested file missing: %v", err)
	}
}

func TestMaterializedName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"go.mod.txt", "go.mod"},
		{"nested/go.mod.txt", "nested/go.mod"},
		{"main.go", "main.go"},
		{"go.mod.txt.bak", "go.mod.txt.bak"},
		{"notgo.mod.txt", "notgo.mod.txt"},
	}
	for _, tt := range tests {
		if got := materializedName(tt.in); got != tt.want {
			t.Errorf("materializedName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestOverlayAssertions(t *testing.T) {
	cases, err := LoadCases(fakeFixtures(), "cases")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := cases[0].MaterializeRepo(dir); err != nil {
		t.Fatal(err)
	}
	if err := cases[0].OverlayAssertions(dir); err != nil {
		t.Fatalf("OverlayAssertions: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a_test.go")); err != nil {
		t.Errorf("assertion file missing: %v", err)
	}
}

// A case with no hidden assertion could only ever report the pipeline's opinion
// of itself, which is the thing this package exists to check independently.
func TestLoadCases_RejectsCaseWithNoAssertion(t *testing.T) {
	fsys := fstest.MapFS{
		"cases/x/case.json":    &fstest.MapFile{Data: []byte(`{"name":"x","task":"t"}`)},
		"cases/x/repo/a.go":    &fstest.MapFile{Data: []byte("package p\n")},
		"cases/x/assert/.keep": &fstest.MapFile{Data: []byte("")},
	}
	_, err := LoadCases(fsys, "cases")
	if err == nil {
		t.Fatal("a case with no assertion files was accepted")
	}
	if !strings.Contains(err.Error(), "ground truth") {
		t.Errorf("err = %v, want it to explain there would be no ground truth", err)
	}
}

func TestLoadCases_Rejects(t *testing.T) {
	tests := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{
			"no name",
			fstest.MapFS{
				"cases/x/case.json":        &fstest.MapFile{Data: []byte(`{"task":"t"}`)},
				"cases/x/repo/a.go":        &fstest.MapFile{Data: []byte("package p\n")},
				"cases/x/assert/a_test.go": &fstest.MapFile{Data: []byte("package p\n")},
			},
			"no name",
		},
		{
			"no task",
			fstest.MapFS{
				"cases/x/case.json":        &fstest.MapFile{Data: []byte(`{"name":"x"}`)},
				"cases/x/repo/a.go":        &fstest.MapFile{Data: []byte("package p\n")},
				"cases/x/assert/a_test.go": &fstest.MapFile{Data: []byte("package p\n")},
			},
			"no task",
		},
		{
			"malformed json",
			fstest.MapFS{
				"cases/x/case.json":        &fstest.MapFile{Data: []byte(`{not json`)},
				"cases/x/repo/a.go":        &fstest.MapFile{Data: []byte("package p\n")},
				"cases/x/assert/a_test.go": &fstest.MapFile{Data: []byte("package p\n")},
			},
			"parse",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadCases(tt.fsys, "cases")
			if err == nil {
				t.Fatal("invalid case was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadCases_EmptyDirIsAnError(t *testing.T) {
	if _, err := LoadCases(fstest.MapFS{"cases/.keep": &fstest.MapFile{}}, "cases"); err == nil {
		t.Fatal("an empty case directory was accepted")
	}
}

// Dotfiles and underscore files must be ignored exactly as //go:embed ignores
// them, so a fixture tree means the same thing from either source.
func TestReadTree_IgnoresDotAndUnderscoreFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"r/real.go":      &fstest.MapFile{Data: []byte("package p\n")},
		"r/.keep":        &fstest.MapFile{Data: []byte("")},
		"r/.DS_Store":    &fstest.MapFile{Data: []byte("junk")},
		"r/_scratch.go":  &fstest.MapFile{Data: []byte("package p\n")},
		"r/_hidden/x.go": &fstest.MapFile{Data: []byte("package p\n")},
	}
	got, err := readTree(fsys, "r")
	if err != nil {
		t.Fatalf("readTree: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d files (%v), want only real.go", len(got), keys(got))
	}
	if _, ok := got["real.go"]; !ok {
		t.Errorf("files = %v, want real.go", keys(got))
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
