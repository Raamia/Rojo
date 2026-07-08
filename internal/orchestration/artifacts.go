package orchestration

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Raamia/Rojo/internal/events"
)

// ArtifactDiff is the name a job's patch is stored under. It is a plain unified
// diff, so `curl .../diff | git apply` works without any unwrapping.
const ArtifactDiff = "patch.diff"

// diffTimeout bounds reading a patch out of a worktree. It runs on a context
// stripped of cancellation, so it needs a deadline of its own or a wedged git
// would hold the worker forever.
const diffTimeout = 30 * time.Second

// ArtifactStore persists a job's outputs. Declared here, near its consumer, so
// the processor can be tested without a filesystem.
//
// This is deliberately not part of jobs.JobRepository: a job record is small,
// rewritten on every status change, and held in memory by List. A diff is
// large, written once, and read rarely. Storing one inside the other would make
// every status update rewrite a patch, and every listing load every patch.
type ArtifactStore interface {
	WriteArtifact(jobID, name string, data []byte) error
}

// captureDiff saves an attempt's patch as a job artifact.
//
// This is the actual product of a job — a reviewable patch a human can read and
// apply — so it has to happen while the worktree still exists. The cleanup
// deferred at the top of Process removes that worktree on the way out, which is
// why this runs inside the pipeline rather than after it.
//
// It never fails the job. Work that was done and verified stays done; losing
// the artifact is worth a loud log, not the retraction of a passing result.
func (p *Processor) captureDiff(ctx context.Context, log *slog.Logger, jobID string, c *candidate) {
	if p.Artifacts == nil || p.Workspaces == nil || c == nil || c.ws == nil {
		return
	}

	// Stripped of cancellation, and separately bounded. By the time a job that
	// timed out or was cancelled reaches here its context is already dead, and
	// reading the diff through it fails — losing the patch in exactly the cases
	// where seeing what the job was doing matters most. Cleanup solves the same
	// problem the same way.
	diffCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), diffTimeout)
	defer cancel()

	diff, err := p.Workspaces.Diff(diffCtx, c.ws)
	if err != nil {
		log.Error("capture diff", "variant", c.index, "err", err)
		return
	}
	if strings.TrimSpace(diff) == "" {
		// An empty diff means the attempt changed nothing. Saying so explicitly
		// is more useful than an artifact that silently does not exist, because
		// "the model produced no change" and "the diff was lost" look identical
		// from the outside otherwise.
		log.Warn("nothing to capture: attempt produced no changes", "variant", c.index)
		p.emit(ctx, jobID, events.TypeDiffCaptured, map[string]any{
			"variant": c.index, "bytes": 0, "empty": true,
		})
		return
	}

	if err := p.Artifacts.WriteArtifact(jobID, ArtifactDiff, []byte(diff)); err != nil {
		log.Error("write diff artifact", "variant", c.index, "err", err)
		return
	}
	log.Info("diff captured", "variant", c.index, "bytes", len(diff))

	// The event carries the diff's shape, never its body. Payloads are appended
	// to the event log and fanned out to every WebSocket subscriber through a
	// bounded buffer; a multi-megabyte patch on that path would evict other
	// events and disconnect slow clients. The body is fetched deliberately,
	// from the artifact endpoint.
	p.emit(ctx, jobID, events.TypeDiffCaptured, map[string]any{
		"variant": c.index,
		"bytes":   len(diff),
		"files":   changedFiles(diff),
	})
}

// changedFiles lists the paths a unified diff touches.
//
// Read from the "+++ b/path" lines, which name the post-image; a deletion
// reports /dev/null there, so the "--- a/path" line is used as the fallback.
// This is for display only — the diff itself remains the source of truth.
func changedFiles(diff string) []string {
	var (
		out  []string
		seen = map[string]bool{}
		prev string
	)
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			prev = trimDiffPath(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			path := trimDiffPath(strings.TrimPrefix(line, "+++ "))
			if path == "" {
				path = prev // a deletion: the post-image is /dev/null
			}
			if path != "" && !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
		}
	}
	return out
}

func trimDiffPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(s, "a/"), "b/")
}
