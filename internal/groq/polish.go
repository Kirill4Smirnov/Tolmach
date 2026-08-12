package groq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
)

const punctuationSystemPrompt = `Ты — строгий корректор русской расшифровки речи.
Разрешено менять только регистр букв, знаки препинания и деление на абзацы.
Запрещено добавлять, удалять, заменять, переставлять или исправлять слова.
Сохраняй повторы, слова-паразиты, мат, оговорки, имена и числа ровно как во входном тексте.
Верни только обработанный текст без пояснений, кавычек и Markdown.`

type PolishRequest struct {
	Text  string
	Model string
}

// PolishPunctuation asks a text model to restore punctuation, then verifies
// locally that no lexical content changed. The model output is never trusted
// without this guard.
func (c *Client) PolishPunctuation(ctx context.Context, input PolishRequest) (string, error) {
	if strings.TrimSpace(input.Text) == "" {
		return "", errors.New("empty transcript")
	}
	if strings.TrimSpace(input.Model) == "" {
		return "", errors.New("empty polish model")
	}
	payload := struct {
		Model       string `json:"model"`
		Temperature int    `json:"temperature"`
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}{Model: input.Model, Temperature: 0}
	payload.Messages = append(payload.Messages,
		struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "system", Content: punctuationSystemPrompt},
		struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "user", Content: input.Text},
	)
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode polish request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create polish request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send polish request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseSize+1))
	if err != nil {
		return "", fmt.Errorf("read polish response: %w", err)
	}
	if len(responseBody) > maximumResponseSize {
		return "", errors.New("Groq polish response exceeds 16 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", decodeAPIError(response.StatusCode, responseBody, response.Header.Get("Retry-After"))
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return "", fmt.Errorf("decode polish response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("Groq polish response contains no choices")
	}
	polished := strings.TrimSpace(decoded.Choices[0].Message.Content)
	if err := ValidatePunctuationOnly(input.Text, polished); err != nil {
		return "", err
	}
	return polished, nil
}

// ValidatePunctuationOnly ensures that words and numbers remain exactly the
// same, ignoring case and separators. Changing е to ё is intentionally treated
// as a lexical change: this stage may punctuate, but may not correct words.
func ValidatePunctuationOnly(original, polished string) error {
	want := lexicalTokens(original)
	got := lexicalTokens(polished)
	if len(want) != len(got) {
		return fmt.Errorf("polish rejected: word count changed from %d to %d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("polish rejected: word %d changed", i+1)
		}
	}
	return nil
}

func lexicalTokens(text string) []string {
	var tokens []string
	var token strings.Builder
	flush := func() {
		if token.Len() > 0 {
			tokens = append(tokens, token.String())
			token.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			token.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return tokens
}
