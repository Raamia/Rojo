package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEnv writes a .env in a temp dir and returns its path.
func writeEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvFile_SetsVariables(t *testing.T) {
	path := writeEnv(t, "ROJO_TEST_A=one\nROJO_TEST_B=two\n")
	t.Cleanup(func() { os.Unsetenv("ROJO_TEST_A"); os.Unsetenv("ROJO_TEST_B") })

	res, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if os.Getenv("ROJO_TEST_A") != "one" || os.Getenv("ROJO_TEST_B") != "two" {
		t.Errorf("variables not set: A=%q B=%q", os.Getenv("ROJO_TEST_A"), os.Getenv("ROJO_TEST_B"))
	}
	if len(res.Set) != 2 {
		t.Errorf("reported %v set, want both", res.Set)
	}
}

// The rule that matters. A container or systemd unit sets real environment
// variables; a stale .env quietly overriding them would be a nasty way to lose
// an afternoon.
func TestLoadEnvFile_RealEnvironmentWins(t *testing.T) {
	t.Setenv("ROJO_TEST_WINNER", "from-environment")
	path := writeEnv(t, "ROJO_TEST_WINNER=from-file\n")

	res, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := os.Getenv("ROJO_TEST_WINNER"); got != "from-environment" {
		t.Errorf("value = %q, want the real environment to win", got)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "ROJO_TEST_WINNER" {
		t.Errorf("skipped = %v, want the overridden name reported", res.Skipped)
	}
}

// Most deployments have no .env at all.
func TestLoadEnvFile_MissingFileIsNotAnError(t *testing.T) {
	res, err := LoadEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("a missing file should be fine: %v", err)
	}
	if res.Path != "" || len(res.Set) != 0 {
		t.Errorf("unexpected result %+v", res)
	}
}

func TestLoadEnvFile_Format(t *testing.T) {
	content := strings.Join([]string{
		"# a comment",
		"",
		"   ",
		"PLAIN=value",
		"export EXPORTED=yes",
		`DQUOTED="  spaced  "`,
		`SQUOTED='single'`,
		"SPACED  =  trimmed  ",
		"INLINE=value # trailing comment",
		"HASHINVALUE=abc#def",
		`QUOTEDHASH="literal # here"`,
		"EMPTY=",
		"EQUALSINVALUE=a=b=c",
		"not a definition line",
	}, "\n")
	path := writeEnv(t, content)

	names := []string{"PLAIN", "EXPORTED", "DQUOTED", "SQUOTED", "SPACED",
		"INLINE", "HASHINVALUE", "QUOTEDHASH", "EMPTY", "EQUALSINVALUE"}
	t.Cleanup(func() {
		for _, n := range names {
			os.Unsetenv(n)
		}
	})

	if _, err := LoadEnvFile(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	want := map[string]string{
		"PLAIN":    "value",
		"EXPORTED": "yes",
		// Quotes are how a value keeps its whitespace.
		"DQUOTED": "  spaced  ",
		"SQUOTED": "single",
		"SPACED":  "trimmed",
		// The usual dotenv behaviour for an unquoted value.
		"INLINE": "value",
		// A '#' not preceded by a space is part of the value — a password or a
		// URL fragment must survive.
		"HASHINVALUE": "abc#def",
		"QUOTEDHASH":  "literal # here",
		"EMPTY":       "",
		// Only the first '=' splits, so a value may contain more.
		"EQUALSINVALUE": "a=b=c",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// The file usually holds API keys, so nothing about it may print a value.
func TestEnvFileResult_LogsNamesNeverValues(t *testing.T) {
	t.Setenv("ROJO_TEST_PRESET", "already-set")
	path := writeEnv(t, "ROJO_TEST_SECRET=sk-SUPERSECRET\nROJO_TEST_PRESET=ignored-value\n")
	t.Cleanup(func() { os.Unsetenv("ROJO_TEST_SECRET") })

	res, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rendered := res.LogValue().String()
	for _, secret := range []string{"sk-SUPERSECRET", "ignored-value", "already-set"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("log rendering leaked a value (%q): %s", secret, rendered)
		}
	}
	if !strings.Contains(rendered, "ROJO_TEST_SECRET") {
		t.Errorf("log rendering should name what it set: %s", rendered)
	}
}

// A file of API keys that other users can read is worth saying out loud.
func TestLoadEnvFile_FlagsPermissiveModes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not restrict reads")
	}
	path := writeEnv(t, "ROJO_TEST_PERM=x\n")
	t.Cleanup(func() { os.Unsetenv("ROJO_TEST_PERM") })

	res, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if res.WorldReadable {
		t.Errorf("0600 should not be flagged")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("ROJO_TEST_PERM")
	res, err = LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.WorldReadable {
		t.Errorf("0644 holds secrets readable by others and was not flagged")
	}
}

// A line with a value but no name is a real mistake, not a stray comment.
func TestLoadEnvFile_RejectsANamelessAssignment(t *testing.T) {
	path := writeEnv(t, "=novalue\n")
	if _, err := LoadEnvFile(path); err == nil {
		t.Fatal("a nameless assignment was accepted")
	}
}

// End to end: keys in a file reach Load() the same as exported ones.
func TestLoadEnvFile_FeedsConfigLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "OPENAI_API_KEY=sk-from-file\nROJO_WORKER_COUNT=7\nROJO_DATA_DIR=" + dir + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Unsetenv("OPENAI_API_KEY")
		os.Unsetenv("ROJO_WORKER_COUNT")
		os.Unsetenv("ROJO_DATA_DIR")
	})
	// Make sure nothing inherited from the real environment decides this.
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("ROJO_PROVIDER")

	if _, err := LoadEnvFile(path); err != nil {
		t.Fatalf("load env file: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.OpenAIAPIKey != "sk-from-file" {
		t.Errorf("key = %q, want the one from the file", cfg.OpenAIAPIKey)
	}
	if cfg.ResolvedProvider() != ProviderOpenAI {
		t.Errorf("provider = %q, want openai inferred from the file's key", cfg.ResolvedProvider())
	}
	if cfg.WorkerCount != 7 {
		t.Errorf("worker count = %d, want 7 from the file", cfg.WorkerCount)
	}
}
