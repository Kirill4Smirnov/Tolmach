package soniox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maximumResponseSize = 64 << 20

type Client struct {
	apiKey       string
	baseURL      string
	httpClient   *http.Client
	pollInterval time.Duration
}

type Options struct {
	BaseURL      string
	HTTPClient   *http.Client
	PollInterval time.Duration
}

type Request struct {
	FilePath                     string
	Model                        string
	LanguageHints                []string
	LanguageHintsStrict          bool
	EnableSpeakerDiarization     bool
	EnableLanguageIdentification bool
	Context                      string
	Terms                        []string
}

type Result struct {
	FileID          string
	TranscriptionID string
	Raw             []byte
	Text            string
	Diarized        string
	Turns           []Turn
	Languages       []string
	Duration        float64
}

type Turn struct {
	Speaker string
	Start   float64
	End     float64
	Text    string
}

type APIError struct {
	StatusCode int
	Type       string
	Message    string
	RequestID  string
	RetryAfter string
}

func (e *APIError) Error() string {
	message := fmt.Sprintf("Soniox API returned HTTP %d", e.StatusCode)
	if e.Type != "" {
		message += " (" + e.Type + ")"
	}
	if e.Message != "" {
		message += ": " + e.Message
	}
	if e.RequestID != "" {
		message += " [request " + e.RequestID + "]"
	}
	if e.RetryAfter != "" {
		message += " (Retry-After: " + e.RetryAfter + ")"
	}
	return message
}

type fileResponse struct {
	ID string `json:"id"`
}

type transcriptionResponse struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	AudioDurationMS int64  `json:"audio_duration_ms"`
	ErrorType       string `json:"error_type"`
	ErrorMessage    string `json:"error_message"`
}

type transcriptResponse struct {
	ID     string  `json:"id"`
	Text   string  `json:"text"`
	Tokens []Token `json:"tokens"`
}

type Token struct {
	Text              string  `json:"text"`
	StartMS           int64   `json:"start_ms"`
	EndMS             int64   `json:"end_ms"`
	Confidence        float64 `json:"confidence"`
	Speaker           string  `json:"speaker"`
	Language          string  `json:"language"`
	IsAudioEvent      bool    `json:"is_audio_event"`
	TranslationStatus string  `json:"translation_status"`
}

func NewClient(apiKey string, options Options) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("empty Soniox API key")
	}
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.soniox.com"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Soniox base URL %q", baseURL)
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Minute}
	}
	pollInterval := options.PollInterval
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	return &Client{apiKey: apiKey, baseURL: baseURL, httpClient: httpClient, pollInterval: pollInterval}, nil
}

// Transcribe uploads a local file, starts an async job, waits for it, and
// returns detailed tokens. FileID and TranscriptionID are retained on partial
// failures so callers can still delete remote data.
func (c *Client) Transcribe(ctx context.Context, input Request) (Result, error) {
	if err := validateRequest(input); err != nil {
		return Result{}, err
	}
	fileID, err := c.uploadFile(ctx, input.FilePath)
	if err != nil {
		return Result{}, err
	}
	result := Result{FileID: fileID}
	job, err := c.createTranscription(ctx, fileID, input)
	if err != nil {
		return result, err
	}
	result.TranscriptionID = job.ID
	for !terminalStatus(job.Status) {
		if err := waitContext(ctx, c.pollInterval); err != nil {
			return result, err
		}
		job, err = c.getTranscription(ctx, job.ID)
		if err != nil {
			return result, err
		}
	}
	if job.Status != "completed" {
		message := job.ErrorMessage
		if message == "" {
			message = "job ended with status " + job.Status
		}
		if job.ErrorType != "" {
			message = job.ErrorType + ": " + message
		}
		return result, errors.New("Soniox transcription failed: " + message)
	}
	raw, err := c.getTranscript(ctx, job.ID)
	if err != nil {
		return result, err
	}
	parsed, err := ParseTranscript(raw)
	if err != nil {
		result.Raw = raw
		return result, err
	}
	parsed.FileID = fileID
	parsed.TranscriptionID = job.ID
	parsed.Duration = float64(job.AudioDurationMS) / 1000
	return parsed, nil
}

func (c *Client) Cleanup(ctx context.Context, transcriptionID, fileID string) error {
	var cleanupErrors []error
	if transcriptionID != "" {
		if err := c.deleteResource(ctx, "/v1/transcriptions/"+url.PathEscape(transcriptionID)); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete transcription: %w", err))
		}
	}
	if fileID != "" {
		if err := c.deleteResource(ctx, "/v1/files/"+url.PathEscape(fileID)); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete file: %w", err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func (c *Client) uploadFile(ctx context.Context, filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open input: %w", err)
	}
	defer file.Close()
	reader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeErr)
		part, err := multipartWriter.CreateFormFile("file", filepath.Base(filePath))
		if err == nil {
			_, err = io.Copy(part, file)
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		if err != nil {
			_ = pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		writeErr <- pipeWriter.Close()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/files", reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		return "", err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	response, requestErr := c.httpClient.Do(req)
	if requestErr != nil {
		_ = reader.CloseWithError(requestErr)
		<-writeErr
		return "", fmt.Errorf("upload Soniox file: %w", requestErr)
	}
	body, readErr := readResponse(response)
	_ = reader.Close()
	streamErr := <-writeErr
	if readErr != nil {
		return "", readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", decodeAPIError(response.StatusCode, body, response.Header.Get("Retry-After"))
	}
	if streamErr != nil {
		return "", fmt.Errorf("stream Soniox file: %w", streamErr)
	}
	var uploaded fileResponse
	if err := json.Unmarshal(body, &uploaded); err != nil {
		return "", fmt.Errorf("decode Soniox file response: %w", err)
	}
	if uploaded.ID == "" {
		return "", errors.New("Soniox upload response contains no file ID")
	}
	return uploaded.ID, nil
}

func (c *Client) createTranscription(ctx context.Context, fileID string, input Request) (transcriptionResponse, error) {
	type contextPayload struct {
		Text  string   `json:"text,omitempty"`
		Terms []string `json:"terms,omitempty"`
	}
	payload := struct {
		Model                        string          `json:"model"`
		FileID                       string          `json:"file_id"`
		LanguageHints                []string        `json:"language_hints,omitempty"`
		LanguageHintsStrict          bool            `json:"language_hints_strict,omitempty"`
		EnableSpeakerDiarization     bool            `json:"enable_speaker_diarization"`
		EnableLanguageIdentification bool            `json:"enable_language_identification"`
		Context                      *contextPayload `json:"context,omitempty"`
	}{
		Model: input.Model, FileID: fileID, LanguageHints: input.LanguageHints,
		LanguageHintsStrict:          input.LanguageHintsStrict,
		EnableSpeakerDiarization:     input.EnableSpeakerDiarization,
		EnableLanguageIdentification: input.EnableLanguageIdentification,
	}
	if strings.TrimSpace(input.Context) != "" || len(input.Terms) > 0 {
		payload.Context = &contextPayload{Text: strings.TrimSpace(input.Context), Terms: cleanList(input.Terms)}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return transcriptionResponse{}, err
	}
	var created transcriptionResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/transcriptions", body, &created); err != nil {
		return transcriptionResponse{}, fmt.Errorf("create Soniox transcription: %w", err)
	}
	if created.ID == "" {
		return transcriptionResponse{}, errors.New("Soniox response contains no transcription ID")
	}
	return created, nil
}

func (c *Client) getTranscription(ctx context.Context, id string) (transcriptionResponse, error) {
	var job transcriptionResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/transcriptions/"+url.PathEscape(id), nil, &job); err != nil {
		return transcriptionResponse{}, fmt.Errorf("get Soniox transcription: %w", err)
	}
	return job, nil
}

func (c *Client) getTranscript(ctx context.Context, id string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/transcriptions/"+url.PathEscape(id)+"/transcript", nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get Soniox transcript: %w", err)
	}
	body, err := readResponse(response)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeAPIError(response.StatusCode, body, response.Header.Get("Retry-After"))
	}
	return body, nil
}

func (c *Client) deleteResource(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	body, err := readResponse(response)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response.StatusCode, body, response.Header.Get("Retry-After"))
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body []byte, target any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	c.authorize(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	responseBody, err := readResponse(response)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response.StatusCode, responseBody, response.Header.Get("Retry-After"))
	}
	if target != nil && len(responseBody) > 0 {
		if err := json.Unmarshal(responseBody, target); err != nil {
			return fmt.Errorf("decode Soniox response: %w", err)
		}
	}
	return nil
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
}

func ParseTranscript(raw []byte) (Result, error) {
	var decoded transcriptResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Result{}, fmt.Errorf("decode Soniox transcript: %w", err)
	}
	var turns []Turn
	languages := make(map[string]bool)
	for _, token := range decoded.Tokens {
		if token.Language != "" {
			languages[token.Language] = true
		}
		if token.IsAudioEvent || token.TranslationStatus == "translation" {
			continue
		}
		speaker := token.Speaker
		if speaker == "" {
			speaker = "UU"
		}
		if len(turns) == 0 || turns[len(turns)-1].Speaker != speaker {
			turns = append(turns, Turn{Speaker: speaker, Start: float64(token.StartMS) / 1000})
		}
		turn := &turns[len(turns)-1]
		turn.Text += token.Text
		if end := float64(token.EndMS) / 1000; end > turn.End {
			turn.End = end
		}
	}
	languageList := make([]string, 0, len(languages))
	for language := range languages {
		languageList = append(languageList, language)
	}
	sort.Strings(languageList)
	return Result{Raw: raw, Text: strings.TrimSpace(decoded.Text), Turns: turns, Diarized: FormatTurns(turns), Languages: languageList}, nil
}

func FormatTurns(turns []Turn) string {
	var output strings.Builder
	written := 0
	for _, turn := range turns {
		text := strings.TrimSpace(turn.Text)
		if text == "" {
			continue
		}
		if written > 0 {
			output.WriteString("\n\n")
		}
		fmt.Fprintf(&output, "[%s–%s] Speaker %s:\n%s", timestamp(turn.Start), timestamp(turn.End), turn.Speaker, text)
		written++
	}
	return output.String()
}

func validateRequest(input Request) error {
	if strings.TrimSpace(input.FilePath) == "" {
		return errors.New("empty input file path")
	}
	if strings.TrimSpace(input.Model) == "" {
		return errors.New("empty Soniox model")
	}
	if len(input.LanguageHints) > 100 {
		return errors.New("Soniox supports at most 100 language hints")
	}
	return nil
}

func cleanList(values []string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func terminalStatus(status string) bool {
	switch status {
	case "completed", "error", "failed":
		return true
	default:
		return false
	}
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func readResponse(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read Soniox response: %w", err)
	}
	if len(body) > maximumResponseSize {
		return nil, errors.New("Soniox response exceeds 64 MiB")
	}
	return body, nil
}

func decodeAPIError(status int, body []byte, retryAfter string) error {
	var decoded struct {
		Type      string `json:"error_type"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(body, &decoded)
	if decoded.Message == "" {
		decoded.Message = strings.TrimSpace(string(body))
	}
	if decoded.Message == "" {
		decoded.Message = http.StatusText(status)
	}
	if len(decoded.Message) > 1000 {
		decoded.Message = decoded.Message[:1000] + "…"
	}
	return &APIError{StatusCode: status, Type: decoded.Type, Message: decoded.Message, RequestID: decoded.RequestID, RetryAfter: retryAfter}
}

func PrettyJSON(raw []byte) ([]byte, error) {
	var output bytes.Buffer
	if err := json.Indent(&output, raw, "", "  "); err != nil {
		return nil, err
	}
	output.WriteByte('\n')
	return output.Bytes(), nil
}

func timestamp(seconds float64) string {
	total := int(seconds + 0.5)
	if total >= 3600 {
		return fmt.Sprintf("%02d:%02d:%02d", total/3600, total/60%60, total%60)
	}
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}
