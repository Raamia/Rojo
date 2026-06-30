package config

// Security audit tests for configuration and secret handling.
//
// Tests ASSERT CURRENT BEHAVIOUR. Names ending in _DocumentsGap pin a
// vulnerable behaviour so the suite stays green while `go test -v` prints the
// gap.

import (
	"fmt"
	"strings"
	"testing"
)

// CRITICAL: authentication is opt-in. Load() reads ROJO_AUTH_TOKEN with a
// plain os.Getenv (config.go:31) and Validate() (config.go:41-55) never checks
// it, so an unset or typo'd variable starts a fully open server. cmd/api/main.go
// then skips the TokenAuth middleware entirely and logs no warning.
func TestSecurity_AuthTokenIsOptionalAndUnvalidated_DocumentsGap(t *testing.T) {
	t.Setenv("ROJO_AUTH_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("VULNERABILITY FIXED? Load now fails without a token: %v", err)
	}
	if cfg.AuthToken != "" {
		t.Fatalf("unexpected token %q", cfg.AuthToken)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("VULNERABILITY FIXED? Validate now rejects an empty token: %v", err)
	}
	t.Log("GAP: Load() + Validate() both succeed with no auth token. main.go's " +
		"`if cfg.AuthToken != \"\"` then omits TokenAuth, so the default deployment is unauthenticated " +
		"and nothing in the startup path says so.")

	// A short/guessable token is equally acceptable.
	t.Setenv("ROJO_AUTH_TOKEN", "a")
	cfg2, err := Load()
	if err != nil || cfg2.AuthToken != "a" {
		t.Fatalf("unexpected: cfg=%v err=%v", cfg2.AuthToken, err)
	}
	t.Log("GAP: a 1-character token passes validation; there is no minimum length/entropy check, " +
		"and no support for token rotation or multiple tokens.")
}

// A zero or negative bucket can never issue a token, so the service refuses
// every request with 429 — a self-inflicted outage from one typo, and one that
// reads as "the server is broken" rather than "the config is wrong". Startup is
// where that has to be caught.
func TestSecurity_RateLimitConfigIsValidated(t *testing.T) {
	for _, tc := range []struct{ burst, rps string }{
		{"0", "5"},
		{"-5", "5"},
		{"30", "0"},
		{"30", "-1"},
	} {
		t.Run("burst="+tc.burst+" rps="+tc.rps, func(t *testing.T) {
			t.Setenv("ROJO_RATE_LIMIT_BURST", tc.burst)
			t.Setenv("ROJO_RATE_LIMIT_RPS", tc.rps)
			if _, err := Load(); err == nil {
				t.Errorf("burst=%s rps=%s was accepted; it refuses every request", tc.burst, tc.rps)
			}
		})
	}
}

// Malformed numeric/duration environment variables are swallowed by
// getEnvInt/getEnvFloat/getEnvDuration (config.go:64-89), which drop the parse
// error and fall back to the default. A typo in a security-relevant limit is
// therefore invisible.
func TestSecurity_MalformedEnvValuesSilentlyFallBack_DocumentsGap(t *testing.T) {
	t.Setenv("ROJO_RATE_LIMIT_BURST", "3O") // letter O, not zero
	t.Setenv("ROJO_RATE_LIMIT_RPS", "five")
	t.Setenv("ROJO_WORKER_COUNT", "lots")
	t.Setenv("ROJO_SHUTDOWN_TIMEOUT", "15") // missing unit -> not a valid duration

	cfg, err := Load()
	if err != nil {
		t.Fatalf("VULNERABILITY FIXED? malformed values now rejected: %v", err)
	}
	if cfg.RateLimitBurst != 30 || cfg.RateLimitRPS != 5 || cfg.WorkerCount != 4 {
		t.Fatalf("unexpected fallbacks: %+v", cfg)
	}
	t.Logf("GAP: every malformed value was silently replaced by its default "+
		"(burst=%d rps=%v workers=%d shutdown=%s). An operator who mistypes a limit gets no error "+
		"and no log line.", cfg.RateLimitBurst, cfg.RateLimitRPS, cfg.WorkerCount, cfg.ShutdownTimeout)
}

// FIXED: Config implements slog.LogValuer and Stringer, so both secrets are
// replaced with [set]/[unset] before reaching a log line. main.go now logs the
// effective configuration at startup, which is exactly the call site that made
// this worth fixing rather than documenting.
func TestSecurity_ConfigRedactsSecrets(t *testing.T) {
	cfg := Config{
		AuthToken:       "SECRET-BEARER-TOKEN",
		AnthropicAPIKey: "sk-ant-SECRET",
		HTTPAddr:        ":8080",
	}

	for _, rendered := range []string{fmt.Sprintf("%+v", cfg), fmt.Sprintf("%v", cfg), cfg.String()} {
		for _, secret := range []string{"SECRET-BEARER-TOKEN", "sk-ant-SECRET"} {
			if strings.Contains(rendered, secret) {
				t.Errorf("rendered config leaked %q: %s", secret, rendered)
			}
		}
		if !strings.Contains(rendered, "[set]") {
			t.Errorf("rendered config should mark secrets as [set]: %s", rendered)
		}
	}

	// An unset secret is distinguishable from a set one, which is what makes
	// the startup log useful for spotting a dropped key.
	empty := fmt.Sprintf("%+v", Config{HTTPAddr: ":8080"})
	if !strings.Contains(empty, "[unset]") {
		t.Errorf("an unset secret should render as [unset]: %s", empty)
	}
}
