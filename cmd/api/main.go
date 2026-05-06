package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
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

	ctx := context.Background()
	pool.Start(ctx)

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

	logger.Info("api listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server failed", "err", err)
		os.Exit(1)
	}
}
