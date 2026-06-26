package config

import (
	"errors"
	"fmt"
	"net"
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
	JobTimeout      time.Duration
	FanoutVariants  int
	AuthToken       string
	RateLimitBurst  int
	RateLimitRPS    float64
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getEnv("ROJO_HTTP_ADDR", DefaultHTTPAddr),
		DBURL:           os.Getenv("ROJO_DB_URL"),
		QueueBuffer:     getEnvInt("ROJO_QUEUE_BUFFER", 64),
		WorkerCount:     getEnvInt("ROJO_WORKER_COUNT", 4),
		WorktreeBaseDir: getEnv("ROJO_WORKTREE_DIR", "/tmp/rojo-worktrees"),
		ShutdownTimeout: getEnvDuration("ROJO_SHUTDOWN_TIMEOUT", 15*time.Second),
		JobTimeout:      getEnvDuration("ROJO_JOB_TIMEOUT", 30*time.Minute),
		FanoutVariants:  getEnvInt("ROJO_FANOUT_VARIANTS", 1),
		AuthToken:       os.Getenv("ROJO_AUTH_TOKEN"),
		RateLimitBurst:  getEnvInt("ROJO_RATE_LIMIT_BURST", 30),
		RateLimitRPS:    getEnvFloat("ROJO_RATE_LIMIT_RPS", 5),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

const DefaultHTTPAddr = "127.0.0.1:8080"

// IsPubliclyBound reports whether HTTPAddr accepts connections from beyond
// this machine.
func (c Config) IsPubliclyBound() bool {
	host, _, err := net.SplitHostPort(c.HTTPAddr)
	if err != nil {
		return true
	}
	if host == "" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() {
		return true
	}
	return !ip.IsLoopback()
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
	if c.RateLimitBurst <= 0 {
		return fmt.Errorf("ROJO_RATE_LIMIT_BURST must be positive, got %d (zero refuses every request)", c.RateLimitBurst)
	}
	if c.RateLimitRPS <= 0 {
		return fmt.Errorf("ROJO_RATE_LIMIT_RPS must be positive, got %v (zero never refills the bucket)", c.RateLimitRPS)
	}
	if c.JobTimeout <= 0 {
		return fmt.Errorf("ROJO_JOB_TIMEOUT must be positive, got %s", c.JobTimeout)
	}
	if c.FanoutVariants < 1 {
		return fmt.Errorf("ROJO_FANOUT_VARIANTS must be at least 1, got %d", c.FanoutVariants)
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
