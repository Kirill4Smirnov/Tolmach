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
)

type TranslateRequest struct {
	Text           string
	TargetLanguage string
	Model          string
}

// Translate translates transcript text without summarizing or annotating it.
// The result is intentionally cached by the bot because translation is a paid
// model call and identical retries should not incur additional cost.
func (c *Client) Translate(ctx context.Context, input TranslateRequest) (string, error) {
	if strings.TrimSpace(input.Text) == "" {
		return "", errors.New("empty text to translate")
	}
	if strings.TrimSpace(input.TargetLanguage) == "" {
		return "", errors.New("empty target language")
	}
	if strings.TrimSpace(input.Model) == "" {
		return "", errors.New("empty translation model")
	}
	system := `You translate speech transcripts faithfully. Preserve meaning, paragraph boundaries, speaker labels, timestamps, repetitions, names, numbers, and uncertainty. Do not summarize, explain, censor, or add Markdown. Return only the translated transcript. Target language: ` + input.TargetLanguage
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
		}{Role: "system", Content: system},
		struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "user", Content: input.Text},
	)
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode translation request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create translation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send translation request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseSize+1))
	if err != nil {
		return "", fmt.Errorf("read translation response: %w", err)
	}
	if len(responseBody) > maximumResponseSize {
		return "", errors.New("Groq translation response exceeds 16 MiB")
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
		return "", fmt.Errorf("decode translation response: %w", err)
	}
	if len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return "", errors.New("Groq translation response contains no text")
	}
	return strings.TrimSpace(decoded.Choices[0].Message.Content), nil
}
