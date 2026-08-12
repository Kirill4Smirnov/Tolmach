package groq

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTranslate(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/openai/v1/chat/completions" || request.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Role, Content string
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "model-1" || len(payload.Messages) != 2 || !strings.Contains(payload.Messages[0].Content, "English") {
			t.Fatalf("payload = %#v", payload)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(
			`{"choices":[{"message":{"content":"Hello!"}}]}`)), Request: request}, nil
	})
	client, _ := NewClient("secret", Options{BaseURL: "https://groq.test/openai/v1", HTTPClient: &http.Client{Transport: transport}})
	result, err := client.Translate(context.Background(), TranslateRequest{Text: "Привет!", TargetLanguage: "English", Model: "model-1"})
	if err != nil || result != "Hello!" {
		t.Fatalf("result=%q err=%v", result, err)
	}
}
