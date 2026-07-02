package orchestration

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Raamia/Rojo/internal/agents/implementor"
	"github.com/Raamia/Rojo/internal/repocontext"
)

// Bounds on how much source is sent to the model. Every byte here is paid for
// on each attempt, and a fan-out multiplies that by the variant count.
//
// The file count matches repocontext.DefaultMaxFiles on purpose: the selector
// ranks up to that many files as worth showing, and a lower cap here silently
// discarded the tail of its ranking — the two limits drifting apart meant the
// planner and the implementor were shown different repositories. The byte
// ceiling works out to roughly 100k tokens of source per attempt, which at
// Opus input pricing is about fifty cents a variant: enough context to edit a
// real package, expensive enough that raising it should be a decision, not a
// drift.
const (
	maxSourceFiles     = repocontext.DefaultMaxFiles
	maxSourceFileBytes = 64 << 10  // skip anything larger; it is generated or vendored
	maxSourceTotalByte = 384 << 10 // hard ceiling across all files
)

// readSources loads the selected files out of the job's worktree.
//
// Reading from the worktree rather than the source repository matters: the
// implementor must see, and edit, the same checkout its changes land in. On a
// revision pass that is the already-modified copy, so the model sees its own
// previous attempt rather than the original.
//
// Anything unreadable is skipped rather than failing the job — a missing file
// is one less piece of context, not a reason to abandon the work.
func readSources(worktree string, paths []string) []implementor.SourceFile {
	out := make([]implementor.SourceFile, 0, len(paths))
	total := 0

	for _, rel := range paths {
		if len(out) >= maxSourceFiles || total >= maxSourceTotalByte {
			break
		}
		// The paths come from git ls-files on this repository, but they still
		// build a filesystem path, so refuse anything that would climb out.
		if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			continue
		}
		full := filepath.Join(worktree, filepath.Clean(rel))

		info, err := os.Stat(full)
		if err != nil || info.IsDir() || info.Size() > maxSourceFileBytes {
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		out = append(out, implementor.SourceFile{Path: rel, Content: string(b)})
		total += len(b)
	}
	return out
}
