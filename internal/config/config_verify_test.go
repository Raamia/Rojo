package config

import (
	"testing"
	"time"
)

// allConfigEnvVars is every env var Load() reads. Setting each to "" via
// t.Setenv makes the helpers treat them as unset (they check `ok && v != ""`),
// which forces the documented defaults while keeping automatic cleanup.
var allConfigEnvVars = []string{
	"ROJO_HTTP_ADDR",
	"ROJO_DATA_DIR",
	"ROJO_QUEUE_BUFFER",
	"ROJO_WORKER_COUNT",
	"ROJO_WORKTREE_DIR",
	"ROJO_SHUTDOWN_TIMEOUT",
	"ROJO_JOB_TIMEOUT",
	"ROJO_FANOUT_VARIANTS",
	"ANTHROPIC_API_KEY",
	"ROJO_MODEL",
	"ROJO_AUTH_TOKEN",
	"ROJO_RATE_LIMIT_BURST",
	"ROJO_RATE_LIMIT_RPS",
}

// clearConfigEnv forces every config env var to the "unset" state.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range allConfigEnvVars {
		t.Setenv(k, "")
	}
}

// TestLoad_Defaults verifies that with no env vars set, Load() returns the
// documented default configuration and no error.
func TestLoad_Defaults(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	want := Config{
		HTTPAddr:        DefaultHTTPAddr,
		DataDir:         "./rojo-data",
		QueueBuffer:     64,
		WorkerCount:     4,
		WorktreeBaseDir: "/tmp/rojo-worktrees",
		ShutdownTimeout: 15 * time.Second,
		JobTimeout:      30 * time.Minute,
		FanoutVariants:  1,
		AuthToken:       "",
		RateLimitBurst:  30,
		RateLimitRPS:    5,
	}

	if cfg != want {
		t.Errorf("Load() defaults mismatch:\n got  %+v\n want %+v", cfg, want)
	}
}

// TestLoad_EnvOverrides verifies that every field is read from its env var
// when a valid value is present.
func TestLoad_EnvOverrides(t *testing.T) {
	clearConfigEnv(t)

	t.Setenv("ROJO_HTTP_ADDR", ":9090")
	t.Setenv("ROJO_DATA_DIR", "/var/rojo/data")
	t.Setenv("ROJO_QUEUE_BUFFER", "128")
	t.Setenv("ROJO_WORKER_COUNT", "8")
	t.Setenv("ROJO_WORKTREE_DIR", "/var/rojo/worktrees")
	t.Setenv("ROJO_SHUTDOWN_TIMEOUT", "30s")
	t.Setenv("ROJO_JOB_TIMEOUT", "1m")
	t.Setenv("ROJO_FANOUT_VARIANTS", "3")
	t.Setenv("ROJO_AUTH_TOKEN", "secret-token")
	t.Setenv("ROJO_RATE_LIMIT_BURST", "100")
	t.Setenv("ROJO_RATE_LIMIT_RPS", "12.5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	want := Config{
		HTTPAddr:        ":9090",
		DataDir:         "/var/rojo/data",
		QueueBuffer:     128,
		WorkerCount:     8,
		WorktreeBaseDir: "/var/rojo/worktrees",
		ShutdownTimeout: 30 * time.Second,
		JobTimeout:      time.Minute,
		FanoutVariants:  3,
		AuthToken:       "secret-token",
		RateLimitBurst:  100,
		RateLimitRPS:    12.5,
	}

	if cfg != want {
		t.Errorf("Load() overrides mismatch:\n got  %+v\n want %+v", cfg, want)
	}
}

// TestLoad_BadValuesFallBack verifies the documented silent-fallback behavior:
// unparseable int/duration/float values do NOT error, they revert to defaults.
func TestLoad_BadValuesFallBack(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
		check func(c Config) (got, want any)
	}{
		{
			name:  "bad int queue buffer falls back to 64",
			key:   "ROJO_QUEUE_BUFFER",
			value: "not-a-number",
			check: func(c Config) (any, any) { return c.QueueBuffer, 64 },
		},
		{
			name:  "bad int worker count falls back to 4",
			key:   "ROJO_WORKER_COUNT",
			value: "12x",
			check: func(c Config) (any, any) { return c.WorkerCount, 4 },
		},
		{
			name:  "bad int rate limit burst falls back to 30",
			key:   "ROJO_RATE_LIMIT_BURST",
			value: "abc",
			check: func(c Config) (any, any) { return c.RateLimitBurst, 30 },
		},
		{
			name:  "bad duration shutdown timeout falls back to 15s",
			key:   "ROJO_SHUTDOWN_TIMEOUT",
			value: "not-a-duration",
			check: func(c Config) (any, any) { return c.ShutdownTimeout, 15 * time.Second },
		},
		{
			name:  "bad duration job timeout falls back to 30m",
			key:   "ROJO_JOB_TIMEOUT",
			value: "30",
			check: func(c Config) (any, any) { return c.JobTimeout, 30 * time.Minute },
		},
		{
			name:  "bad float rate limit rps falls back to 5",
			key:   "ROJO_RATE_LIMIT_RPS",
			value: "five",
			check: func(c Config) (any, any) { return c.RateLimitRPS, float64(5) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.key, tc.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() with bad %s=%q returned error (should silently fall back): %v", tc.key, tc.value, err)
			}

			got, want := tc.check(cfg)
			if got != want {
				t.Errorf("bad %s=%q: got %v, want fallback %v", tc.key, tc.value, got, want)
			}
		})
	}
}

// validConfig returns a Config that passes Validate(), used as a baseline that
// individual cases mutate into an invalid state.
func validConfig() Config {
	return Config{
		HTTPAddr:        DefaultHTTPAddr,
		DataDir:         "./rojo-data",
		QueueBuffer:     64,
		WorkerCount:     4,
		WorktreeBaseDir: "/tmp/rojo-worktrees",
		ShutdownTimeout: 15 * time.Second,
		JobTimeout:      time.Minute,
		FanoutVariants:  1,
		RateLimitBurst:  30,
		RateLimitRPS:    5,
	}
}

// TestValidate exercises the documented Validate() rules: non-empty HTTPAddr,
// positive QueueBuffer, positive WorkerCount, positive ShutdownTimeout.
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{
			name:    "valid config",
			mutate:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "empty DataDir",
			mutate:  func(c *Config) { c.DataDir = "" },
			wantErr: true,
		},
		{
			name:    "empty HTTPAddr",
			mutate:  func(c *Config) { c.HTTPAddr = "" },
			wantErr: true,
		},
		{
			name:    "zero QueueBuffer",
			mutate:  func(c *Config) { c.QueueBuffer = 0 },
			wantErr: true,
		},
		{
			name:    "negative QueueBuffer",
			mutate:  func(c *Config) { c.QueueBuffer = -1 },
			wantErr: true,
		},
		{
			name:    "zero WorkerCount",
			mutate:  func(c *Config) { c.WorkerCount = 0 },
			wantErr: true,
		},
		{
			name:    "negative WorkerCount",
			mutate:  func(c *Config) { c.WorkerCount = -3 },
			wantErr: true,
		},
		{
			name:    "zero ShutdownTimeout",
			mutate:  func(c *Config) { c.ShutdownTimeout = 0 },
			wantErr: true,
		},
		{
			name:    "negative ShutdownTimeout",
			mutate:  func(c *Config) { c.ShutdownTimeout = -time.Second },
			wantErr: true,
		},
		{
			// A zero job timeout would mean unbounded execution, which is the
			// bug the setting exists to prevent — so it is rejected outright
			// rather than treated as "no limit".
			name:    "zero JobTimeout",
			mutate:  func(c *Config) { c.JobTimeout = 0 },
			wantErr: true,
		},
		{
			name:    "negative JobTimeout",
			mutate:  func(c *Config) { c.JobTimeout = -time.Minute },
			wantErr: true,
		},
		{
			name:    "zero FanoutVariants",
			mutate:  func(c *Config) { c.FanoutVariants = 0 },
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)

			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() = nil, want error for %s", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil for %s", err, tc.name)
			}
		})
	}
}
