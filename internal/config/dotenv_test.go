package config

import (
	"net/http"
	"net/url"
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

func TestLoadedHTTPSProxyIsUsedByGoHTTPTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("HTTPS_PROXY=http://127.0.0.1:7890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "")
	if err := os.Unsetenv("HTTPS_PROXY"); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	requestURL, _ := url.Parse("https://api.groq.com/openai/v1/models")
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: requestURL})
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:7890" {
		t.Fatalf("proxy URL = %v", proxyURL)
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}
