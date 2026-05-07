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
	"github.com/Raamia/Rojo/internal/jobs"
	"github.com/Raamia/Rojo/internal/orchestration"
	"github.com/Raamia/Rojo/internal/queue"
	"github.com/Raamia/Rojo/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	repo := jobs.NewInMemoryRepository()
	q := queue.New(64)
	processor := orchestration.NewProcessor(repo)
	pool := worker.NewPool(4, q, processor)
	handler := api.NewJobsHandler(repo, q)

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	pool.Start(workerCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		api.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/v1/jobs", handler.Create)
	mux.HandleFunc("GET /api/v1/jobs", handler.List)
	mux.HandleFunc("GET /api/v1/jobs/{jobID}", handler.Get)

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown failed", "err", err)
	}

	cancelWorkers()
	q.Close()
	pool.Wait()

	logger.Info("shutdown complete")
}
