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

func TestPolishPunctuation(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "llama-3.3-70b-versatile" {
			t.Fatalf("model = %v", body["model"])
		}
		response := `{"choices":[{"message":{"content":"Слушай, я не знаю. Можно к трём?"}}]}`
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(bytes.NewBufferString(response)), Request: request,
		}, nil
	})
	client, _ := NewClient("key", Options{
		BaseURL: "https://groq.test/v1", HTTPClient: &http.Client{Transport: transport},
	})
	got, err := client.PolishPunctuation(context.Background(), PolishRequest{
		Text: "слушай я не знаю можно к трём", Model: "llama-3.3-70b-versatile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Слушай, я не знаю. Можно к трём?" {
		t.Fatalf("got %q", got)
	}
}

func TestValidatePunctuationOnly(t *testing.T) {
	tests := []struct {
		name     string
		original string
		polished string
		valid    bool
	}{
		{"punctuation and case", "привет как дела", "Привет! Как дела?", true},
		{"paragraph", "раз два три", "Раз, два.\n\nТри!", true},
		{"hyphen separator", "как-то так", "Как то так.", true},
		{"added word", "привет мир", "Привет, мой мир!", false},
		{"corrected word", "ихний дом", "Их дом.", false},
		{"yo is lexical", "все", "Всё.", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePunctuationOnly(test.original, test.polished)
			if (err == nil) != test.valid {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPolishRejectsRewording(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, request.Body)
		response := `{"choices":[{"message":{"content":"Это совершенно новый текст."}}]}`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
	})
	client, _ := NewClient("key", Options{BaseURL: "https://groq.test/v1", HTTPClient: &http.Client{Transport: transport}})
	_, err := client.PolishPunctuation(context.Background(), PolishRequest{Text: "старый текст", Model: "model"})
	if err == nil || !strings.Contains(err.Error(), "polish rejected") {
		t.Fatalf("error = %v", err)
	}
}
