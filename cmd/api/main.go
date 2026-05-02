package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Raamia/Rojo/internal/api"
	"github.com/Raamia/Rojo/internal/jobs"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	repo := jobs.NewInMemoryRepository()
	handler := api.NewJobsHandler(repo)

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
	}

	logger.Info("api listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server failed", "err", err)
		os.Exit(1)
	}
}
