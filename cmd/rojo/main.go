// Command rojo is the CLI for a running Rojo server: submit a task, watch it
// plan, implement, verify and review, and receive the patch.
//
// It is a separate binary from cmd/api on purpose. The server owns execution
// and enforcement; this owns presentation. They talk over the same HTTP API any
// other client would use, so the CLI has no privileged path — if something is
// only possible from here, that is an API gap to fix, not a shortcut to take.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `rojo — run AI coding tasks against a local repository

Usage:
  rojo run [-repo DIR] [-apply] "task description"
  rojo list [-limit N]
  rojo get JOB_ID
  rojo events JOB_ID
  rojo diff JOB_ID
  rojo cancel JOB_ID

Global flags (before the subcommand):
  -server URL   API address        (default $ROJO_SERVER or http://127.0.0.1:8080)
  -token TOKEN  bearer token       (default $ROJO_AUTH_TOKEN)

run flags:
  -repo DIR     repository to work on (default: the current directory)
  -apply        apply the resulting patch to the repository when the job completes

The patch is written to stdout; progress goes to stderr. That split is what
makes  rojo run "fix X" > fix.patch  work while still showing progress.

Exit codes: 0 completed, 1 failed or error, 2 cancelled.`

// run is main with its dependencies visible, so tests can drive the whole CLI
// without a subprocess.
func run(args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("rojo", flag.ContinueOnError)
	global.SetOutput(stderr)
	server := global.String("server", envOr("ROJO_SERVER", "http://127.0.0.1:8080"), "API address")
	token := global.String("token", os.Getenv("ROJO_AUTH_TOKEN"), "bearer token")
	if err := global.Parse(args); err != nil {
		return 1
	}
	rest := global.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, usage)
		return 1
	}
	c := newClient(strings.TrimRight(*server, "/"), *token)

	switch cmd, cmdArgs := rest[0], rest[1:]; cmd {
	case "run":
		return cmdRun(c, cmdArgs, stdout, stderr)
	case "list":
		return cmdList(c, cmdArgs, stdout, stderr)
	case "get":
		return cmdGet(c, cmdArgs, stdout, stderr)
	case "events":
		return cmdEvents(c, cmdArgs, stdout, stderr)
	case "diff":
		return cmdDiff(c, cmdArgs, stdout, stderr)
	case "cancel":
		return cmdCancel(c, cmdArgs, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s\n", cmd, usage)
		return 1
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// pollInterval balances liveness against hammering a loopback server. Events
// appear within half a second of happening, which reads as live to a human.
const pollInterval = 400 * time.Millisecond

func cmdRun(c *client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "repository to work on (default: current directory)")
	apply := fs.Bool("apply", false, "apply the resulting patch to the repository")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		fmt.Fprintln(stderr, `run needs a task, e.g.  rojo run "add a retry to the fetcher"`)
		return 1
	}

	repoPath, err := resolveRepo(*repo)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	j, err := c.createJob(task, repoPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stderr, "job %s created for %s\n", j.ID, repoPath)

	// First Ctrl-C asks the server to cancel and keeps watching — the job ends
	// `cancelled` and its worktree is cleaned up. A second Ctrl-C gives up on
	// watching; the server still finishes the cancellation on its own.
	interrupt := make(chan os.Signal, 2)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	go func() {
		<-interrupt
		fmt.Fprintln(stderr, "\ncancelling the job (Ctrl-C again to stop watching)...")
		if err := c.cancelJob(j.ID); err != nil {
			fmt.Fprintln(stderr, "cancel request failed:", err)
		}
		<-interrupt
		fmt.Fprintln(stderr, "no longer watching; the server will finish cancelling")
		os.Exit(2)
	}()

	final, err := watch(c, j.ID, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	// The patch goes to stdout in every terminal outcome that produced one —
	// a failed job's patch is what a human reads to see what went wrong.
	patch, err := c.jobDiff(j.ID)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
	} else if patch != "" {
		if *apply && final.Status == "completed" {
			if err := gitApply(repoPath, patch); err != nil {
				fmt.Fprintln(stderr, "error applying patch:", err)
				fmt.Fprint(stdout, patch) // still hand it over rather than losing it
				return 1
			}
			fmt.Fprintln(stderr, "patch applied to", repoPath)
		} else {
			fmt.Fprint(stdout, patch)
		}
	}

	switch final.Status {
	case "completed":
		return 0
	case "cancelled":
		return 2
	default:
		return 1
	}
}

// watch polls the job's event history and prints each new event once, until the
// job reaches a terminal status.
//
// Polling the persisted history instead of holding the WebSocket open is a
// deliberate v1 choice: the history endpoint replays from the start, so a job
// that finished before we connected still tells its whole story, and there is
// no reconnect state to get wrong. The WebSocket exists for clients that need
// lower latency than 400ms.
func watch(c *client, id string, stderr io.Writer) (job, error) {
	seen := 0
	for {
		evs, err := c.jobEvents(id, seen)
		if err != nil {
			return job{}, err
		}
		for _, e := range evs {
			if line := renderEvent(e); line != "" {
				fmt.Fprintln(stderr, line)
			}
		}
		seen += len(evs)

		j, err := c.getJob(id)
		if err != nil {
			return job{}, err
		}
		switch j.Status {
		case "completed", "failed", "cancelled":
			return j, nil
		}
		time.Sleep(pollInterval)
	}
}

// resolveRepo turns the -repo flag (or the working directory) into the absolute
// path the server needs, and fails early when it is visibly not a repository —
// a wrong path is better caught here than as a failed job three states in.
func resolveRepo(flagValue string) (string, error) {
	dir := flagValue
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(abs, ".git")); err != nil {
		return "", fmt.Errorf("%s is not the root of a git repository (no .git found)", abs)
	}
	return abs, nil
}

// gitApply pipes the patch into `git apply` in the repository. The CLI runs on
// the user's own machine with the user's own git — the server-side allowlist
// discipline is about what jobs may execute, not what the user may.
func gitApply(repoPath, patch string) error {
	cmd := exec.Command("git", "apply", "-")
	cmd.Dir = repoPath
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply: %v\n%s", err, out)
	}
	return nil
}

func cmdList(c *client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("limit", 20, "how many jobs to show")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	list, total, err := c.listJobs(*limit)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no jobs yet")
		return 0
	}
	for _, j := range list {
		fmt.Fprintf(stdout, "%s  %-12s  %s  %s\n",
			j.ID, j.Status, j.CreatedAt.Local().Format("2006-01-02 15:04"), oneLine(j.Task, 60))
	}
	if total != "" && total != fmt.Sprint(len(list)) {
		fmt.Fprintf(stdout, "(%d of %s jobs; -limit to see more)\n", len(list), total)
	}
	return 0
}

func cmdGet(c *client, args []string, stdout, stderr io.Writer) int {
	id, ok := oneID(args, stderr, "get")
	if !ok {
		return 1
	}
	j, err := c.getJob(id)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "id:       %s\nstatus:   %s\nrepo:     %s\ntask:     %s\ncreated:  %s\nupdated:  %s\n",
		j.ID, j.Status, j.RepoPath, j.Task,
		j.CreatedAt.Local().Format(time.RFC3339), j.UpdatedAt.Local().Format(time.RFC3339))
	return 0
}

func cmdEvents(c *client, args []string, stdout, stderr io.Writer) int {
	id, ok := oneID(args, stderr, "events")
	if !ok {
		return 1
	}
	evs, err := c.jobEvents(id, 0) // the events subcommand shows the whole history
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	for _, e := range evs {
		if line := renderEvent(e); line != "" {
			fmt.Fprintln(stdout, line)
		}
	}
	return 0
}

func cmdDiff(c *client, args []string, stdout, stderr io.Writer) int {
	id, ok := oneID(args, stderr, "diff")
	if !ok {
		return 1
	}
	patch, err := c.jobDiff(id)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if patch == "" {
		fmt.Fprintln(stderr, "no patch for this job (it may still be running, or produced no changes)")
		return 1
	}
	fmt.Fprint(stdout, patch)
	return 0
}

func cmdCancel(c *client, args []string, stdout, stderr io.Writer) int {
	id, ok := oneID(args, stderr, "cancel")
	if !ok {
		return 1
	}
	if err := c.cancelJob(id); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, "cancel requested for", id)
	return 0
}

func oneID(args []string, stderr io.Writer, cmd string) (string, bool) {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintf(stderr, "usage: rojo %s JOB_ID\n", cmd)
		return "", false
	}
	return args[0], true
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}
