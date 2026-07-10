package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Raamia/Rojo/internal/agents/implementor"
	"github.com/Raamia/Rojo/internal/agents/model"
	"github.com/Raamia/Rojo/internal/agents/planner"
	"github.com/Raamia/Rojo/internal/agents/reviewer"
	"github.com/Raamia/Rojo/internal/api"
	"github.com/Raamia/Rojo/internal/config"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/execution"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/metrics"
	"github.com/Raamia/Rojo/internal/orchestration"
	"github.com/Raamia/Rojo/internal/queue"
	"github.com/Raamia/Rojo/internal/repocontext"
	"github.com/Raamia/Rojo/internal/storage/filestore"
	"github.com/Raamia/Rojo/internal/verification"
	"github.com/Raamia/Rojo/internal/worker"
	"github.com/Raamia/Rojo/internal/workspace"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid config", "err", err)
		os.Exit(2)
	}

	logger.Info("configuration loaded", "config", cfg)

	store, err := filestore.New(cfg.DataDir)
	if err != nil {
		logger.Error("open data dir", "path", cfg.DataDir, "err", err)
		os.Exit(1)
	}
	// Released on the graceful path; on any other exit the kernel releases the
	// flock when the process dies, so a crash never wedges the next start.
	defer store.Close()
	var repo jobs.JobRepository = store
	logger.Info("data dir opened", "path", cfg.DataDir, "jobs_loaded", store.Loaded())

	q := queue.New(cfg.QueueBuffer)
	canceller := orchestration.NewCanceller()
	// Every event is written to the job's log before it is fanned out, so the
	// history endpoint and the WebSocket stream never disagree.
	var bus events.Bus = events.NewPersistingBus(events.NewInProcessBus(), store)
	// Git runs through the allowlisted runner rather than raw exec, so the
	// orchestrator can only ever invoke git, with a bounded runtime.
	gitRunner := execution.NewSafeRunner(
		execution.NewExecRunner(),
		execution.NewAllowlist("git"),
		5*time.Minute,
	)
	workspaces := workspace.NewGitWorkspaceManager(gitRunner, cfg.WorktreeBaseDir)
	logger.Info("worktree base dir", "path", cfg.WorktreeBaseDir)
	logger.Info("verification detects the repo's stack", "commands", verification.AutoCommands())

	// Verification gets its own runner and allowlist: it may invoke test
	// toolchains, never git, and vice versa. The allowlist is exactly the set
	// of commands detection can choose from — the repository under test picks
	// which preset applies (by containing go.mod, package.json, ...) but can
	// never name a command.
	verifyRunner := execution.NewSafeRunner(
		execution.NewExecRunner(),
		execution.NewAllowlist(verification.AutoCommands()...),
		10*time.Minute,
	)
	verifier := verification.NewAutoRunner(verifyRunner)

	reg := metrics.New()

	processor := orchestration.NewProcessor(repo, canceller, bus)
	processor.Metrics = reg
	processor.Workspaces = workspaces
	processor.Verifier = verifier
	processor.Artifacts = store

	// The planner only runs when a model is configured. Without a key the
	// pipeline still does useful work — isolate, verify, report — it just does
	// not plan, so an unset key degrades the service rather than breaking it.
	if cfg.AnthropicAPIKey != "" {
		modelClient := metrics.InstrumentModel(model.NewAnthropicClient(model.AnthropicOptions{
			APIKey:  cfg.AnthropicAPIKey,
			Model:   cfg.ModelID,
			Timeout: cfg.JobTimeout,
		}), reg)
		processor.Planner = planner.NewPlanner(modelClient)
		processor.Context = repocontext.NewSelector(gitRunner)
		processor.Implementor = implementor.NewAgent(modelClient)
		processor.Reviewer = reviewer.New(modelClient)
		logger.Info("agents enabled", "model", modelName(cfg.ModelID),
			"max_revisions", orchestration.MaxRevisions)
	} else {
		logger.Warn("ANTHROPIC_API_KEY not set: planning, implementation and review are disabled")
	}
	processor.JobTimeout = cfg.JobTimeout
	processor.Variants = cfg.FanoutVariants
	if cfg.FanoutVariants > 1 {
		logger.Info("fan-out enabled", "variants", cfg.FanoutVariants)
	}
	pool := worker.NewPool(cfg.WorkerCount, q, processor)
	handler := api.NewJobsHandler(repo, q, canceller, bus)

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	pool.Start(workerCtx)

	// Reconcile persisted state with the empty in-process queue before the
	// server accepts anything new, so recovered work is not competing with
	// fresh submissions for queue slots.
	recoverer := &orchestration.Recoverer{
		Repo:            repo,
		Queue:           q,
		Bus:             bus,
		Workspaces:      workspaces,
		WorktreeBaseDir: cfg.WorktreeBaseDir,
		Logger:          logger,
	}
	if report, err := recoverer.Recover(context.Background()); err != nil {
		logger.Error("job recovery failed; starting anyway", "err", err)
	} else if !report.IsZero() {
		logger.Info("recovered jobs from a previous run",
			"requeued", report.Requeued,
			"failed_interrupted", report.FailedInterrupted,
			"worktrees_reclaimed", report.WorktreesReclaimed,
			"still_queued", report.Unqueueable)
	}

	mux := http.NewServeMux()
	health := api.NewHealthHandler()
	// Writability, not reachability: a read-only or full data directory is the
	// failure that makes this instance useless while it still answers HTTP.
	health.Register("store", func(context.Context) error { return store.Health() })
	health.Info = func() map[string]any {
		return map[string]any{"queue_depth": q.Len(), "workers": cfg.WorkerCount}
	}
	mux.HandleFunc("GET /healthz", health.Health)
	mux.HandleFunc("POST /api/v1/jobs", handler.Create)
	mux.HandleFunc("GET /api/v1/jobs", handler.List)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}", handler.Get)
	mux.HandleFunc("POST /api/v1/jobs/{jobID}/cancel", handler.Cancel)

	stream := api.NewStreamHandler(bus)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}/stream", stream.Stream)

	history := api.NewEventsHandler(store)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}/events", history.History)

	diffs := api.NewDiffHandler(repo, store)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}/diff", diffs.Diff)

	metricsHandler := api.NewMetricsHandler(reg, func() map[string]any {
		return map[string]any{"queue_depth": q.Len(), "workers": cfg.WorkerCount}
	})
	mux.HandleFunc("GET /api/v1/metrics", metricsHandler.Metrics)

	var handlerChain http.Handler = mux
	handlerChain = api.RecoverMiddleware()(handlerChain)
	handlerChain = api.LoggerMiddleware(logger)(handlerChain)
	limiter := api.NewRateLimiter(cfg.RateLimitBurst, cfg.RateLimitRPS)
	if cfg.TrustProxyHeader {
		limiter.TrustProxyHeader()
		logger.Info("rate limiting keys on X-Forwarded-For (ROJO_TRUST_PROXY_HEADER=true)")
	}
	handlerChain = limiter.Middleware()(handlerChain)
	if cfg.AuthToken != "" {
		handlerChain = api.TokenAuth(cfg.AuthToken)(handlerChain)
	} else {
		// A dropped or misspelled ROJO_AUTH_TOKEN silently leaves the whole API
		// — including job submission, which executes code — open to anyone who
		// can reach the port. Make that state impossible to deploy unnoticed.
		if cfg.IsPubliclyBound() {
			// The dangerous combination, and the only one worth shouting about:
			// reachable from the network *and* unauthenticated, on a service
			// whose job-creation endpoint executes code.
			logger.Error("REACHABLE FROM THE NETWORK WITH NO AUTHENTICATION",
				"addr", cfg.HTTPAddr,
				"risk", "anyone who can reach this address can run code on this machine",
				"fix", "set ROJO_AUTH_TOKEN, or bind loopback with ROJO_HTTP_ADDR=127.0.0.1:8080")
		} else {
			logger.Warn("ROJO_AUTH_TOKEN not set: authentication is disabled",
				"addr", cfg.HTTPAddr,
				"mitigation", "bound to loopback, so only this machine can reach it")
		}
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handlerChain,
		ReadHeaderTimeout: 5 * time.Second,
		// Bound how long a client may take to send a request and how long an
		// idle keep-alive connection may sit. Without these a slowloris client
		// dribbling one byte at a time holds a connection open indefinitely.
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 120 * time.Second,
		// WriteTimeout is deliberately left unset: /stream is a long-lived
		// WebSocket and a server-wide write deadline would sever it. The
		// stream handler applies its own per-write deadline instead.
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stop:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-serverErr:
		logger.Error("server failed", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown failed", "err", err)
	}

	cancelWorkers()
	q.Close()
	pool.Wait()

	logger.Info("shutdown complete")
}

// modelName reports the model that will actually be used, so the startup log
// is accurate when ROJO_MODEL is unset and the client falls back to its default.
func modelName(configured string) string {
	if configured != "" {
		return configured
	}
	return string(model.DefaultModel)
}
