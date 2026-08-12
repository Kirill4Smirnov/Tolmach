package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tolmach/internal/output"
)

func TestParseGranularities(t *testing.T) {
	got, err := parseGranularities("segment, word,segment")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "segment" || got[1] != "word" {
		t.Fatalf("got %#v", got)
	}
	if _, err := parseGranularities("speaker"); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestDefaultRussianPromptIsNonEmpty(t *testing.T) {
	if defaultRussianPrompt == "" {
		t.Fatal("default Russian prompt must not be empty")
	}
}

func TestRunStopsBatchAfterForbidden(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.ogg")
	second := filepath.Join(dir, "second.ogg")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		_, _ = io.Copy(io.Discard, request.Body)
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"Forbidden"}}`)),
			Request:    request,
		}, nil
	})
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous }()
	t.Setenv("GROQ_API_KEY", "test-key")

	exitCode := run([]string{
		"--base-url", "https://groq.test/v1",
		"--env-file", filepath.Join(dir, "missing.env"),
		"--out", filepath.Join(dir, "out"),
		first, second,
	})
	if exitCode != 1 {
		t.Fatalf("exit code = %d", exitCode)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestRunSpeechmaticsWritesDiarizationThenDeletesJob(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "dialog.ogg")
	if err := os.WriteFile(input, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	var methods []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		methods = append(methods, request.Method)
		if request.Body != nil {
			_, _ = io.Copy(io.Discard, request.Body)
		}
		body := `{"id":"job-3","status":"done","json-v2":{"format":"2.9","results":[{"type":"word","start_time":0,"end_time":1,"alternatives":[{"content":"Привет","language":"ru","speaker":"S1"}]}]}}`
		status := http.StatusCreated
		if request.Method == http.MethodDelete {
			body = ""
			status = http.StatusNoContent
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Request:    request,
		}, nil
	})
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous }()
	t.Setenv("SPEECHMATICS_API_KEY", "test-key")

	exitCode := run([]string{
		"--provider", "speechmatics",
		"--base-url", "https://speech.test/v2",
		"--env-file", filepath.Join(dir, "missing.env"),
		"--out", filepath.Join(dir, "out"),
		input,
	})
	if exitCode != 0 {
		t.Fatalf("exit code = %d", exitCode)
	}
	if strings.Join(methods, ",") != "POST,DELETE" {
		t.Fatalf("methods = %#v", methods)
	}
	paths := output.ResultPaths(filepath.Join(dir, "out"), input, "speechmatics-enhanced-speaker", "ru")
	for _, path := range []string{paths.Text, paths.JSON, paths.Speakers} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing output %s: %v", path, err)
		}
	}
}

func TestRunSonioxWritesDiarizationThenDeletesRemoteData(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "dialog.ogg")
	if err := os.WriteFile(input, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		if request.Body != nil {
			_, _ = io.Copy(io.Discard, request.Body)
		}
		body := ""
		status := http.StatusOK
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/files":
			body, status = `{"id":"file-3"}`, http.StatusCreated
		case "POST /v1/transcriptions":
			body, status = `{"id":"tr-3","status":"completed","audio_duration_ms":1000}`, http.StatusCreated
		case "GET /v1/transcriptions/tr-3/transcript":
			body = `{"id":"tr-3","text":"Привет.","tokens":[{"text":"Привет.","start_ms":0,"end_ms":1000,"speaker":"1","language":"ru"}]}`
		case "DELETE /v1/transcriptions/tr-3", "DELETE /v1/files/file-3":
			status = http.StatusNoContent
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Request:    request,
		}, nil
	})
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous }()
	t.Setenv("SONIOX_API_KEY", "test-key")

	exitCode := run([]string{
		"--provider", "soniox",
		"--base-url", "https://soniox.test",
		"--env-file", filepath.Join(dir, "missing.env"),
		"--out", filepath.Join(dir, "out"),
		input,
	})
	if exitCode != 0 {
		t.Fatalf("exit code = %d", exitCode)
	}
	wantCalls := "POST /v1/files,POST /v1/transcriptions,GET /v1/transcriptions/tr-3/transcript,DELETE /v1/transcriptions/tr-3,DELETE /v1/files/file-3"
	if strings.Join(calls, ",") != wantCalls {
		t.Fatalf("calls = %#v", calls)
	}
	paths := output.ResultPaths(filepath.Join(dir, "out"), input, "soniox-stt-async-v5-speaker", "ru")
	for _, path := range []string{paths.Text, paths.JSON, paths.Speakers} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing output %s: %v", path, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
