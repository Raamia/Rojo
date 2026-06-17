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

	"github.com/Raamia/Rojo/internal/api"
	"github.com/Raamia/Rojo/internal/config"
	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/orchestration"
	"github.com/Raamia/Rojo/internal/queue"
	"github.com/Raamia/Rojo/internal/storage/postgres"
	"github.com/Raamia/Rojo/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid config", "err", err)
		os.Exit(2)
	}

	repo, store, closeRepo, err := buildRepository(logger, cfg)
	if err != nil {
		logger.Error("build repository", "err", err)
		os.Exit(1)
	}
	defer closeRepo()

	q := queue.New(cfg.QueueBuffer)
	canceller := orchestration.NewCanceller()
	var bus events.Bus = events.NewInProcessBus()
	if store != nil {
		bus = events.NewPersistingBus(bus, store)
	}
	processor := orchestration.NewProcessor(repo, canceller, bus)
	pool := worker.NewPool(cfg.WorkerCount, q, processor)
	handler := api.NewJobsHandler(repo, q, canceller, bus)

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	pool.Start(workerCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		api.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/v1/jobs", handler.Create)
	mux.HandleFunc("GET /api/v1/jobs", handler.List)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}", handler.Get)
	mux.HandleFunc("POST /api/v1/jobs/{jobID}/cancel", handler.Cancel)

	stream := api.NewStreamHandler(bus)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}/stream", stream.Stream)

	if store != nil {
		history := api.NewEventsHandler(store)
		mux.HandleFunc("GET /api/v1/jobs/{jobID}/events", history.History)
	}

	var handlerChain http.Handler = mux
	handlerChain = api.LoggerMiddleware(logger)(handlerChain)
	handlerChain = api.NewRateLimiter(cfg.RateLimitBurst, cfg.RateLimitRPS).Middleware()(handlerChain)
	if cfg.AuthToken != "" {
		handlerChain = api.TokenAuth(cfg.AuthToken)(handlerChain)
	} else {
		// A dropped or misspelled ROJO_AUTH_TOKEN silently leaves the whole API
		// — including job submission, which executes code — open to anyone who
		// can reach the port. Make that state impossible to deploy unnoticed.
		logger.Warn("ROJO_AUTH_TOKEN not set: authentication is DISABLED and every endpoint is publicly accessible")
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

func buildRepository(logger *slog.Logger, cfg config.Config) (jobs.JobRepository, events.Store, func(), error) {
	if cfg.DBURL == "" {
		logger.Warn("ROJO_DB_URL not set, using in-memory repository")
		return jobs.NewInMemoryRepository(), nil, func() {}, nil
	}
	pool, err := postgres.NewPool(context.Background(), postgres.Config{
		URL:      cfg.DBURL,
		MaxConns: 8,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	logger.Info("connected to postgres")
	return postgres.NewJobRepository(pool), events.NewPostgresStore(pool), func() { pool.Close() }, nil
}
