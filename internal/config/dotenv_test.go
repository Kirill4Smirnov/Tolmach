package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("# comment\nexport TOLMACH_TEST_ONE=one\nTOLMACH_TEST_TWO=\"two words\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOLMACH_TEST_ONE", "existing")
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TOLMACH_TEST_ONE"); got != "existing" {
		t.Fatalf("existing variable overwritten: %q", got)
	}
	if got := os.Getenv("TOLMACH_TEST_TWO"); got != "two words" {
		t.Fatalf("quoted value = %q", got)
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestSecretPrefersFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET", "from-env")
	t.Setenv("TEST_SECRET_FILE", path)
	value, err := Secret("TEST_SECRET")
	if err != nil || value != "from-file" {
		t.Fatalf("value=%q err=%v", value, err)
	}
}

func TestSecretFallsBackToEnvironment(t *testing.T) {
	t.Setenv("TEST_SECRET", " value ")
	t.Setenv("TEST_SECRET_FILE", "")
	value, err := Secret("TEST_SECRET")
	if err != nil || value != "value" {
		t.Fatalf("value=%q err=%v", value, err)
	}
}
