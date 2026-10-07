//go:build e2e

package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadDotEnv_NilTBDoesNotPanicWhenAbsent guards the TestMain regression.
// TestMain calls loadDotEnv(nil); when .env.test is absent the loader's
// "not found" branch used to call t.Log on a nil testing.TB, panicking the
// whole e2e binary before any test ran.
func TestLoadDotEnv_NilTBDoesNotPanicWhenAbsent(t *testing.T) {
	// Run from a directory whose ancestors hold no .env.test so the loader
	// takes the "not found" path. t.Chdir restores the original cwd.
	t.Chdir(t.TempDir())

	loadDotEnv(nil) // must not panic
}

// TestLoadDotEnv_LoadsFromDotEnvTest is the positive control: it proves the
// loader really reads .env.test, so the nil-TB test above exercises the
// not-found path rather than passing vacuously.
func TestLoadDotEnv_LoadsFromDotEnvTest(t *testing.T) {
	const key = "PB_E2E_DOTENV_LOADER_TEST_KEY"
	os.Unsetenv(key)
	defer os.Unsetenv(key)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env.test"), []byte(key+"=from-dotenv\n"), 0o600); err != nil {
		t.Fatalf("write .env.test: %v", err)
	}
	t.Chdir(dir)

	loadDotEnv(nil)

	if got := os.Getenv(key); got != "from-dotenv" {
		t.Fatalf("loadDotEnv did not load %s: got %q", key, got)
	}
}

// TestEnvTestExample_HasNoLiveAssignments guards .env.test.example: every
// assignment must be commented out. An uncommented TEST_OPENAI_API_KEY=... line
// meant a verbatim copy set a bogus key and the e2e tests then dialled the real
// API with it.
func TestEnvTestExample_HasNoLiveAssignments(t *testing.T) {
	path, ok := findUp(".env.test.example")
	if !ok {
		t.Fatal(".env.test.example not found walking up from the working directory")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "=") {
			t.Errorf("%s:%d: live assignment in example file (comment it out): %q", path, i+1, line)
		}
	}
}

// findUp walks up from the working directory looking for name, returning its
// path. It mirrors the directory walk loadDotEnv uses to locate .env.test.
func findUp(name string) (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
