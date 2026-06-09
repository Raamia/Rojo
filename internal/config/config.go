package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr        string
	DBURL           string
	QueueBuffer     int
	WorkerCount     int
	WorktreeBaseDir string
	ShutdownTimeout time.Duration
	AuthToken       string
	RateLimitBurst  int
	RateLimitRPS    float64
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getEnv("ROJO_HTTP_ADDR", ":8080"),
		DBURL:           os.Getenv("ROJO_DB_URL"),
		QueueBuffer:     getEnvInt("ROJO_QUEUE_BUFFER", 64),
		WorkerCount:     getEnvInt("ROJO_WORKER_COUNT", 4),
		WorktreeBaseDir: getEnv("ROJO_WORKTREE_DIR", "/tmp/rojo-worktrees"),
		ShutdownTimeout: getEnvDuration("ROJO_SHUTDOWN_TIMEOUT", 15*time.Second),
		AuthToken:       os.Getenv("ROJO_AUTH_TOKEN"),
		RateLimitBurst:  getEnvInt("ROJO_RATE_LIMIT_BURST", 30),
		RateLimitRPS:    getEnvFloat("ROJO_RATE_LIMIT_RPS", 5),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.HTTPAddr == "" {
		return errors.New("ROJO_HTTP_ADDR must not be empty")
	}
	if c.QueueBuffer <= 0 {
		return fmt.Errorf("ROJO_QUEUE_BUFFER must be positive, got %d", c.QueueBuffer)
	}
	if c.WorkerCount <= 0 {
		return fmt.Errorf("ROJO_WORKER_COUNT must be positive, got %d", c.WorkerCount)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("ROJO_SHUTDOWN_TIMEOUT must be positive, got %s", c.ShutdownTimeout)
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}
