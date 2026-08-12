package telegram

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientCallsAndDoesNotLeakToken(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/botsecret/getFile" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		return jsonResponse(request, http.StatusOK, `{"ok":true,"result":{"file_id":"f","file_path":"voice/a.ogg"}}`), nil
	})
	client, err := NewClient("secret", Options{BaseURL: "https://telegram.test", HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	file, err := client.GetFile(context.Background(), "f")
	if err != nil || file.FilePath != "voice/a.ogg" {
		t.Fatalf("file=%#v err=%v", file, err)
	}

	badTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, &urlErrorWithToken{message: request.URL.String()}
	})
	bad, _ := NewClient("secret-token", Options{BaseURL: "https://telegram.test", HTTPClient: &http.Client{Transport: badTransport}})
	_, err = bad.GetFile(context.Background(), "f")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestDownloadAndSplitText(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString("audio")), Request: request}, nil
	})
	client, _ := NewClient("secret", Options{BaseURL: "https://telegram.test", HTTPClient: &http.Client{Transport: transport}})
	path := filepath.Join(t.TempDir(), "audio.ogg")
	if err := client.Download(context.Background(), "voice/a.ogg", path, 10); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
	chunks := SplitText("one two three four", 8)
	if strings.Join(chunks, "|") != "one two|three|four" {
		t.Fatalf("chunks=%#v", chunks)
	}
}

type urlErrorWithToken struct{ message string }

func (e *urlErrorWithToken) Error() string { return e.message }

func jsonResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body)), Request: request}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
