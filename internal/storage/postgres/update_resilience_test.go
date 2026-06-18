package postgres

// Resilience characterization test for the postgres UPDATE semantics.
//
// This is a structural test rather than an integration test: it asserts on the
// SQL text itself so it runs without a database. It documents that the write
// path performs an unconditional, last-write-wins overwrite.

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// FAILURE MODE 5 — no optimistic locking in the postgres write path
// ---------------------------------------------------------------------------

// updateJobSQL matches only on the primary key:
//
//	UPDATE jobs SET task=$2, repo_path=$3, status=$4, updated_at=$5 WHERE id=$1
//
// There is no version column (see migrations/0001_jobs.sql), no `WHERE status =
// <expected>` guard, and no `WHERE updated_at = <read value>` guard. Two
// concurrent writers therefore both succeed and the later one wins, exactly as
// the in-memory repository does. RowsAffected can never be 0 for a conflict —
// only for a genuinely missing row — so JobRepository.Update cannot detect one.
func TestResilience_UpdateJobSQLHasNoOptimisticLocking(t *testing.T) {
	sql := strings.ToLower(updateJobSQL)

	whereIdx := strings.Index(sql, "where")
	if whereIdx < 0 {
		t.Fatal("updateJobSQL has no WHERE clause at all")
	}
	where := sql[whereIdx:]

	// The only predicate is the primary key.
	if strings.Count(where, "$") != 1 || !strings.Contains(where, "id = $1") {
		t.Fatalf("WHERE clause = %q — a conflict guard was added, re-baseline this test", strings.TrimSpace(where))
	}
	for _, guard := range []string{"version", "status =", "updated_at ="} {
		if strings.Contains(where, guard) {
			t.Fatalf("WHERE clause now guards on %q — optimistic locking was added, re-baseline this test", guard)
		}
	}

	// And nothing in the statement bumps a revision counter.
	if strings.Contains(sql, "version") {
		t.Fatal("updateJobSQL now references a version column — re-baseline this test")
	}

	t.Log("CONFIRMED: postgres Update is last-write-wins; concurrent updaters clobber each other silently")
}

// The status column is a bare TEXT with no CHECK constraint and no state-machine
// enforcement, so an out-of-order write (e.g. a stale worker persisting
// `implementing` over `completed`) is accepted by the database. Transition
// legality is enforced only in Go, against the writer's own in-memory copy.
func TestResilience_InsertAndUpdateAcceptAnyStatusString(t *testing.T) {
	for name, sql := range map[string]string{
		"insertJobSQL": insertJobSQL,
		"updateJobSQL": updateJobSQL,
	} {
		lowered := strings.ToLower(sql)
		if strings.Contains(lowered, "check") || strings.Contains(lowered, "case when") {
			t.Fatalf("%s now validates status server-side — re-baseline this test", name)
		}
	}
	t.Log("CONFIRMED: the database applies no state-machine constraint; illegal transitions are only caught in Go")
}
