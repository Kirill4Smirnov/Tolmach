package groq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscribe(t *testing.T) {
	mediaPath := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(mediaPath, []byte("fake audio"), 0o600); err != nil {
		t.Fatal(err)
	}

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("model"); got != "whisper-large-v3-turbo" {
			t.Errorf("model = %q", got)
		}
		if got := r.FormValue("language"); got != "ru" {
			t.Errorf("language = %q", got)
		}
		if got := r.MultipartForm.Value["timestamp_granularities[]"]; len(got) != 1 || got[0] != "segment" {
			t.Errorf("timestamp granularities = %#v", got)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		contents, _ := io.ReadAll(file)
		if string(contents) != "fake audio" {
			t.Errorf("file contents = %q", contents)
		}
		var body bytes.Buffer
		_ = json.NewEncoder(&body).Encode(map[string]any{
			"text": "Привет!", "language": "Russian", "duration": 1.25,
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body.Bytes())),
			Request:    r,
		}, nil
	})

	client, err := NewClient("test-key", Options{
		BaseURL:    "https://groq.test/v1",
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Transcribe(context.Background(), Request{
		FilePath: mediaPath, Model: "whisper-large-v3-turbo", Language: "ru",
		ResponseFormat: "verbose_json", TimestampGranularities: []string{"segment"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Привет!" || result.Duration != 1.25 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestTranscribeAPIErrorDoesNotExposeKey(t *testing.T) {
	mediaPath := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(mediaPath, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, r.Body)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"3"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)),
			Request:    r,
		}, nil
	})
	client, _ := NewClient("secret-key", Options{
		BaseURL:    "https://groq.test/v1",
		HTTPClient: &http.Client{Transport: transport},
	})
	_, err := client.Transcribe(context.Background(), Request{
		FilePath: mediaPath, Model: "model", ResponseFormat: "json",
	})
	if err == nil || !strings.Contains(err.Error(), "Retry-After: 3") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Fatal("API key leaked in error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("typed API error = %#v", apiErr)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
