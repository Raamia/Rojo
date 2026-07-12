package config

import (
	"bufio"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
)

// DefaultEnvFile is read from the working directory at startup when present.
const DefaultEnvFile = ".env"

// EnvFileResult reports what LoadEnvFile did, so the caller can log it without
// this package deciding how. Never carries a value — only names.
type EnvFileResult struct {
	// Path is the file that was read, empty if there was none.
	Path string
	// Set are the variables taken from the file, in file order.
	Set []string
	// Skipped are variables the file defined that were already in the real
	// environment, which wins.
	Skipped []string
	// WorldReadable is true when the file's permissions let other users read
	// it, which for a file holding API keys is worth saying out loud.
	WorldReadable bool
}

// LoadEnvFile reads KEY=VALUE lines from path into the process environment.
//
// It exists because running Rojo means having two API keys, an auth token and a
// handful of settings to hand, and `export`-ing them into every shell is both
// tedious and easy to get wrong. A file is the obvious answer, and this is
// thirty lines of the standard library rather than a dependency.
//
// The rule that matters: **a variable already in the real environment wins.**
// The file fills gaps, it never overrides. That is the universal convention,
// and the reason is deployment — a container or a systemd unit sets real
// environment variables, and a stale .env that quietly took precedence over
// them would be a genuinely nasty way to lose an afternoon.
//
// A missing file is not an error. Most deployments have no .env at all.
//
// Format: KEY=VALUE, one per line. Blank lines and lines beginning with # are
// ignored. A leading "export " is tolerated, because people paste those. Values
// may be single- or double-quoted, which is how a value keeps leading or
// trailing spaces, or a literal " #". An unquoted value has an inline " #"
// comment stripped — the usual dotenv behaviour, and safe for keys and paths,
// which do not contain spaces.
func LoadEnvFile(path string) (EnvFileResult, error) {
	res := EnvFileResult{}

	f, err := os.Open(path)
	if err != nil {
		if errorIsNotExist(err) {
			return res, nil // no file is the normal case, not a problem
		}
		return res, fmt.Errorf("open env file %s: %w", path, err)
	}
	defer f.Close()

	if info, statErr := f.Stat(); statErr == nil {
		// 0o077 is "any permission for group or other". A file holding API keys
		// should be 0600; this does not refuse to read it, because that would
		// be obnoxious, but the caller is told so it can say something.
		res.WorldReadable = info.Mode().Perm()&0o077 != 0
	}
	res.Path = path

	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		key, value, ok := parseEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if key == "" {
			return res, fmt.Errorf("%s:%d: line has no variable name", path, line)
		}
		// The real environment wins, so a value already set is left alone.
		if _, present := os.LookupEnv(key); present {
			res.Skipped = append(res.Skipped, key)
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return res, fmt.Errorf("%s:%d: set %s: %w", path, line, key, err)
		}
		res.Set = append(res.Set, key)
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("read env file %s: %w", path, err)
	}
	return res, nil
}

// parseEnvLine splits one line into a key and value. ok is false for blank
// lines and comments, which are skipped rather than being errors.
func parseEnvLine(raw string) (key, value string, ok bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")

	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		// A line with no '=' is not a definition. Skipping beats failing:
		// the file is usually hand-written, and one odd line should not stop
		// the server from starting.
		return "", "", false
	}

	key = strings.TrimSpace(line[:eq])
	value = strings.TrimSpace(line[eq+1:])

	// Quotes are how a value keeps whitespace or a literal '#'.
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			return key, value[1 : len(value)-1], true
		}
	}
	// Unquoted: strip an inline comment. Only " #" counts, so a '#' inside a
	// value (a URL fragment, a password) survives as long as it is not preceded
	// by a space.
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return key, value, true
}

func errorIsNotExist(err error) bool {
	return os.IsNotExist(err) || strings.Contains(err.Error(), fs.ErrNotExist.Error())
}

// LogValue renders the result for a startup line — names only, never values.
// The whole point of the file is that it holds secrets.
func (r EnvFileResult) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("path", r.Path),
		slog.Any("set", r.Set),
		slog.Any("overridden_by_environment", r.Skipped),
	)
}
