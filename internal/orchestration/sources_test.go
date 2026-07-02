package orchestration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Raamia/Rojo/internal/repocontext"
)

// The selector ranks up to DefaultMaxFiles files as worth showing the model.
// If the reader caps lower, the tail of that ranking is silently discarded and
// the planner and implementor see different repositories — which is exactly
// the drift that happened once, so it is pinned.
func TestSourceCaps_MatchTheSelector(t *testing.T) {
	if maxSourceFiles != repocontext.DefaultMaxFiles {
		t.Errorf("maxSourceFiles = %d, but the selector returns up to %d",
			maxSourceFiles, repocontext.DefaultMaxFiles)
	}
}

func TestReadSources_ReadsSelectedFiles(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"main.go":      "package main // main",
		"pkg/lib.go":   "package pkg // lib",
		"unrelated.go": "package main // never selected",
	} {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := readSources(wt, []string{"main.go", "pkg/lib.go", "missing.go"})
	if len(got) != 2 {
		t.Fatalf("got %d files, want 2 (missing.go skipped): %+v", len(got), got)
	}
	if got[0].Path != "main.go" || !strings.Contains(got[0].Content, "// main") {
		t.Errorf("first file = %+v", got[0])
	}
}

func TestReadSources_RefusesEscapes(t *testing.T) {
	wt := t.TempDir()
	secret := filepath.Join(filepath.Dir(wt), "secret.txt")
	if err := os.WriteFile(secret, []byte("credentials"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(secret)

	got := readSources(wt, []string{"../secret.txt", "/etc/hosts"})
	if len(got) != 0 {
		t.Errorf("a path outside the worktree was read: %+v", got)
	}
}

func TestReadSources_EnforcesTheCaps(t *testing.T) {
	wt := t.TempDir()
	var paths []string
	for i := 0; i < maxSourceFiles+10; i++ {
		rel := "f" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".go"
		if err := os.WriteFile(filepath.Join(wt, rel), []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, rel)
	}
	if got := readSources(wt, paths); len(got) > maxSourceFiles {
		t.Errorf("read %d files, cap is %d", len(got), maxSourceFiles)
	}

	// One oversized file is skipped, not truncated: a partial file misleads
	// the model about what the code says.
	big := filepath.Join(wt, "big.go")
	if err := os.WriteFile(big, make([]byte, maxSourceFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range readSources(wt, []string{"big.go"}) {
		if f.Path == "big.go" {
			t.Error("an oversized file was included")
		}
	}
}
