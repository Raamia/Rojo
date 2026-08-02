// Command rojo-bench measures Rojo against a fixed set of small development
// tasks and writes a report the numbers in a claim can be traced back to.
//
// It talks to a running rojo-api over the public HTTP API, so what it measures
// is the system as deployed rather than an in-process approximation of it.
//
//	bin/rojo-api &
//	bin/rojo-bench -price-in 1.25 -price-out 10 -json bench.json
//
// Every case carries a hidden assertion that is not in the repository the job
// sees. That assertion, not the job's own status, decides whether a case
// passed — see internal/benchmark for why that distinction is the point.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Raamia/Rojo/internal/benchmark"
)

// The fixture tree lives under testdata/ so the go tool ignores the .go files
// inside it — they are case material, not part of this module's build — and is
// embedded so the binary carries its own cases and cannot be run against a
// half-copied directory.
//
//go:embed testdata/cases
var fixtures embed.FS

func main() {
	var (
		server   = flag.String("server", envOr("ROJO_SERVER", "http://127.0.0.1:8080"), "rojo-api address")
		token    = flag.String("token", os.Getenv("ROJO_AUTH_TOKEN"), "bearer token, if the server requires one")
		only     = flag.String("only", "", "comma-separated case names to run (default: all)")
		list     = flag.Bool("list", false, "list the cases and exit without running anything")
		timeout  = flag.Duration("case-timeout", benchmark.DefaultCaseTimeout, "per-case timeout")
		poll     = flag.Duration("poll", 500*time.Millisecond, "how often to check a job's status")
		jsonPath = flag.String("json", "", "write the full report as JSON to this path")
		priceIn  = flag.Float64("price-in", 0, "input token price in dollars per million; 0 skips cost")
		priceOut = flag.Float64("price-out", 0, "output token price in dollars per million; 0 skips cost")
	)
	flag.Parse()

	cases, err := benchmark.LoadCases(fixtures, "testdata/cases")
	if err != nil {
		fail("load cases: %v", err)
	}
	if *only != "" {
		if cases, err = benchmark.FilterCases(cases, strings.Split(*only, ",")); err != nil {
			fail("%v", err)
		}
	}

	if *list {
		fmt.Printf("%d cases:\n", len(cases))
		for _, c := range cases {
			fmt.Printf("  %-20s %-8s %s\n", c.Name, c.Difficulty, firstLine(c.Task))
		}
		return
	}

	// Ctrl-C stops after the case in flight rather than mid-case, so the
	// partial report still describes whole cases.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := benchmark.NewClient(strings.TrimRight(*server, "/"), *token)

	// Fail before the first case rather than after its timeout: a wrong address
	// is the commonest way to start a run that was never going to work.
	healthCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Health(healthCtx); err != nil {
		fail("no rojo server at %s: %v\n\nStart one with:  bin/rojo-api", *server, err)
	}

	runner := &benchmark.Runner{
		Client:      client,
		Commands:    benchmark.GitAndGoRunner(*timeout),
		CaseTimeout: *timeout,
		Poll:        *poll,
		Log:         os.Stderr,
	}

	fmt.Fprintf(os.Stderr, "running %d cases against %s\n\n", len(cases), *server)
	started := time.Now()
	results := runner.RunAll(ctx, cases)
	elapsed := time.Since(started)

	summary := benchmark.Summarize(results, benchmark.Prices{
		InputPerMillion:  *priceIn,
		OutputPerMillion: *priceOut,
	})

	fmt.Print(summary.Table())
	fmt.Printf("\n  wall clock             %s\n", elapsed.Round(time.Second))

	if *jsonPath != "" {
		b, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			fail("encode report: %v", err)
		}
		if err := os.WriteFile(*jsonPath, append(b, '\n'), 0o644); err != nil {
			fail("write %s: %v", *jsonPath, err)
		}
		fmt.Printf("  report                 %s\n", *jsonPath)
	}

	// A false success is the one outcome that should never be shrugged off: the
	// system claimed it did the work and did not. Exit non-zero so a scripted
	// run notices, distinctly from ordinary task failures.
	if summary.FalseSuccesses > 0 {
		fmt.Fprintf(os.Stderr, "\n%d case(s) reported success without doing the task\n",
			summary.FalseSuccesses)
		os.Exit(2)
	}
	if summary.HarnessErrors > 0 {
		os.Exit(1)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 70 {
		return s[:69] + "…"
	}
	return s
}
