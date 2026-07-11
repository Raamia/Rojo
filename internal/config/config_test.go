package config

import (
	"os"
	"testing"
)

// Rojo's job endpoint executes code, so whether the listen address is reachable
// from beyond this machine decides how much an unset auth token costs.
func TestIsPubliclyBound(t *testing.T) {
	tests := map[string]bool{
		// Loopback: only this machine.
		"127.0.0.1:8080": false,
		"localhost:8080": false,
		"[::1]:8080":     false,
		"127.0.0.53:80":  false,

		// Every interface.
		":8080":        true,
		"0.0.0.0:8080": true,
		"[::]:8080":    true,

		// A specific reachable address.
		"10.0.0.121:8080":    true,
		"192.168.1.5:8080":   true,
		"example.com:8080":   true,
		"[2001:db8::1]:8080": true,

		// Unparseable: assume the risky answer rather than the convenient one.
		"not a valid address": true,
		"":                    true,
	}
	for addr, want := range tests {
		t.Run(addr, func(t *testing.T) {
			if got := (Config{HTTPAddr: addr}).IsPubliclyBound(); got != want {
				t.Errorf("IsPubliclyBound(%q) = %v, want %v", addr, got, want)
			}
		})
	}
}

// The default must not expose an unauthenticated code-execution endpoint to the
// network. ":8080" — the obvious choice — binds every interface.
func TestDefaultAddrIsLoopbackOnly(t *testing.T) {
	if (Config{HTTPAddr: DefaultHTTPAddr}).IsPubliclyBound() {
		t.Errorf("DefaultHTTPAddr = %q is reachable from the network", DefaultHTTPAddr)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IsPubliclyBound() {
		t.Errorf("the loaded default addr %q is publicly bound", cfg.HTTPAddr)
	}
}

// Exposing it stays possible — it just has to be asked for.
func TestExplicitPublicBindIsHonoured(t *testing.T) {
	t.Setenv("ROJO_HTTP_ADDR", ":8080")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || !cfg.IsPubliclyBound() {
		t.Errorf("addr = %q, publiclyBound = %v", cfg.HTTPAddr, cfg.IsPubliclyBound())
	}
}

// "on"/"yes"/"enabled" are the natural ways to turn a flag on, and silently
// treating them as the default off is the worst kind of quiet for a
// security-relevant switch.
func TestGetEnvBool_AcceptsNaturalSpellings(t *testing.T) {
	on := []string{"1", "t", "true", "TRUE", "on", "On", "yes", "y", "enable", "enabled"}
	off := []string{"0", "f", "false", "off", "no", "n", "disable", "disabled"}
	for _, v := range on {
		t.Setenv("ROJO_TRUST_PROXY_HEADER", v)
		if !getEnvBool("ROJO_TRUST_PROXY_HEADER", false) {
			t.Errorf("%q did not read as true", v)
		}
	}
	for _, v := range off {
		t.Setenv("ROJO_TRUST_PROXY_HEADER", v)
		if getEnvBool("ROJO_TRUST_PROXY_HEADER", true) {
			t.Errorf("%q did not read as false", v)
		}
	}
	// Unset uses the fallback.
	os.Unsetenv("ROJO_TRUST_PROXY_HEADER")
	if !getEnvBool("ROJO_TRUST_PROXY_HEADER", true) {
		t.Error("unset should use the fallback")
	}
}
