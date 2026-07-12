package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr        string
	DataDir         string
	QueueBuffer     int
	WorkerCount     int
	WorktreeBaseDir string
	ShutdownTimeout time.Duration
	JobTimeout      time.Duration
	FanoutVariants  int
	AnthropicAPIKey string
	OpenAIAPIKey    string
	// Provider selects which model backend the agents use: "anthropic",
	// "openai", or "" to infer from whichever key is set.
	Provider       string
	ModelID        string
	AuthToken      string
	RateLimitBurst int
	RateLimitRPS   float64
	// TrustProxyHeader keys rate limiting by X-Forwarded-For. Only enable it
	// when a trusted reverse proxy is what connects to this server; from an
	// untrusted peer the header is attacker-chosen and defeats the limiter.
	TrustProxyHeader bool
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:         getEnv("ROJO_HTTP_ADDR", DefaultHTTPAddr),
		DataDir:          getEnv("ROJO_DATA_DIR", "./rojo-data"),
		QueueBuffer:      getEnvInt("ROJO_QUEUE_BUFFER", 64),
		WorkerCount:      getEnvInt("ROJO_WORKER_COUNT", 4),
		WorktreeBaseDir:  getEnv("ROJO_WORKTREE_DIR", "/tmp/rojo-worktrees"),
		ShutdownTimeout:  getEnvDuration("ROJO_SHUTDOWN_TIMEOUT", 15*time.Second),
		JobTimeout:       getEnvDuration("ROJO_JOB_TIMEOUT", 30*time.Minute),
		FanoutVariants:   getEnvInt("ROJO_FANOUT_VARIANTS", 1),
		AnthropicAPIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		OpenAIAPIKey:     os.Getenv("OPENAI_API_KEY"),
		Provider:         strings.ToLower(strings.TrimSpace(getEnv("ROJO_PROVIDER", ""))),
		ModelID:          getEnv("ROJO_MODEL", ""),
		AuthToken:        os.Getenv("ROJO_AUTH_TOKEN"),
		RateLimitBurst:   getEnvInt("ROJO_RATE_LIMIT_BURST", 30),
		RateLimitRPS:     getEnvFloat("ROJO_RATE_LIMIT_RPS", 5),
		TrustProxyHeader: getEnvBool("ROJO_TRUST_PROXY_HEADER", false),
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

// LogValue redacts the secrets before Config reaches a log line.
//
// Config holds an auth token and an API key in plain string fields. Without
// this, any slog.Any("config", cfg) — including the startup line below —
// would print both. Implementing slog.LogValuer means the redaction travels
// with the type instead of relying on every call site to remember.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("http_addr", c.HTTPAddr),
		slog.String("data_dir", c.DataDir),
		slog.Int("queue_buffer", c.QueueBuffer),
		slog.Int("worker_count", c.WorkerCount),
		slog.String("worktree_dir", c.WorktreeBaseDir),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.Duration("job_timeout", c.JobTimeout),
		slog.Int("fanout_variants", c.FanoutVariants),
		slog.Int("rate_limit_burst", c.RateLimitBurst),
		slog.Float64("rate_limit_rps", c.RateLimitRPS),
		slog.String("provider", c.ResolvedProvider()),
		slog.String("model", c.ModelID),
		slog.String("auth_token", redacted(c.AuthToken)),
		slog.String("anthropic_api_key", redacted(c.AnthropicAPIKey)),
		slog.String("openai_api_key", redacted(c.OpenAIAPIKey)),
	)
}

// String routes fmt verbs through the same redaction, so a stray %v or %+v
// cannot leak what LogValue is careful to hide.
func (c Config) String() string {
	return c.LogValue().String()
}

func redacted(secret string) string {
	if secret == "" {
		return "[unset]"
	}
	return "[set]"
}

// Providers Rojo can talk to. The agents are provider-agnostic — they depend
// on model.Client — so this is the only place the choice exists.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// ResolvedProvider reports which backend the agents will use, or "" when no key
// is configured and the model-driven stages are disabled.
//
// With ROJO_PROVIDER unset the answer is inferred from whichever key is
// present, because that is nearly always unambiguous and asking someone to set
// two variables to say one thing is a needless step. Anthropic wins a tie only
// so the behaviour of an existing deployment does not change the day an
// OPENAI_API_KEY happens to appear in the environment; ROJO_PROVIDER is how you
// say otherwise.
func (c Config) ResolvedProvider() string {
	switch c.Provider {
	case ProviderAnthropic, ProviderOpenAI:
		return c.Provider
	}
	if c.AnthropicAPIKey != "" {
		return ProviderAnthropic
	}
	if c.OpenAIAPIKey != "" {
		return ProviderOpenAI
	}
	return ""
}

// ProviderAPIKey returns the key for the resolved provider.
func (c Config) ProviderAPIKey() string {
	switch c.ResolvedProvider() {
	case ProviderAnthropic:
		return c.AnthropicAPIKey
	case ProviderOpenAI:
		return c.OpenAIAPIKey
	}
	return ""
}

func (c Config) Validate() error {
	if c.HTTPAddr == "" {
		return errors.New("ROJO_HTTP_ADDR must not be empty")
	}
	if c.DataDir == "" {
		return errors.New("ROJO_DATA_DIR must not be empty")
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
	// An unrecognised provider is refused rather than quietly ignored: falling
	// back to inference would run jobs against a model the operator did not
	// choose, and a typo like "opanai" would look like it worked.
	switch c.Provider {
	case "", ProviderAnthropic, ProviderOpenAI:
	default:
		return fmt.Errorf("ROJO_PROVIDER must be %q or %q, got %q", ProviderAnthropic, ProviderOpenAI, c.Provider)
	}
	// Naming a provider whose key is missing is a misconfiguration worth
	// catching at startup, not one agent call into the first job.
	if c.Provider != "" && c.ProviderAPIKey() == "" {
		return fmt.Errorf("ROJO_PROVIDER=%s but its API key is not set", c.Provider)
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

// getEnvBool parses a boolean env var, accepting more than strconv.ParseBool.
//
// ParseBool rejects "on", "yes" and "enabled" — the exact words an operator
// reaches for to turn a flag on. Silently treating those as the default means
// someone who sets ROJO_TRUST_PROXY_HEADER=on gets it *off* with no sign, which
// for a security-relevant flag is the worst kind of quiet. The accepted
// vocabulary is widened, case-insensitively, so the natural spellings work.
// A genuinely unrecognised value still falls back, consistent with the other
// typed vars here.
func getEnvBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "on", "yes", "y", "enable", "enabled":
		return true
	case "0", "f", "false", "off", "no", "n", "disable", "disabled":
		return false
	default:
		return fallback
	}
}
