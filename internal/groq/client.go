package groq

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
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL      = "https://api.groq.com/openai/v1"
	maximumResponseSize = 16 << 20
)

type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type Options struct {
	BaseURL    string
	HTTPClient *http.Client
}

type Request struct {
	FilePath               string
	Model                  string
	Language               string
	Prompt                 string
	ResponseFormat         string
	TimestampGranularities []string
	Temperature            float64
}

type Result struct {
	Raw      []byte
	Text     string
	Language string
	Duration float64
}

// APIError represents a non-2xx response returned by Groq. Callers can inspect
// StatusCode without parsing human-readable error text.
type APIError struct {
	StatusCode int
	Message    string
	RetryAfter string
}

func (e *APIError) Error() string {
	if e.RetryAfter != "" {
		return fmt.Sprintf("Groq API returned HTTP %d: %s (Retry-After: %s)", e.StatusCode, e.Message, e.RetryAfter)
	}
	return fmt.Sprintf("Groq API returned HTTP %d: %s", e.StatusCode, e.Message)
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func NewClient(apiKey string, options Options) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("empty Groq API key")
	}
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Groq base URL %q", baseURL)
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{apiKey: apiKey, baseURL: baseURL, httpClient: httpClient}, nil
}

// Transcribe streams the media file as multipart data. It does not load the
// recording into memory, which is important for video notes and small servers.
func (c *Client) Transcribe(ctx context.Context, input Request) (Result, error) {
	if input.FilePath == "" {
		return Result{}, errors.New("empty input file path")
	}
	if input.Model == "" {
		return Result{}, errors.New("empty model")
	}
	if input.ResponseFormat != "json" && input.ResponseFormat != "verbose_json" {
		return Result{}, fmt.Errorf("unsupported response format %q", input.ResponseFormat)
	}

	file, err := os.Open(input.FilePath)
	if err != nil {
		return Result{}, fmt.Errorf("open input: %w", err)
	}
	defer file.Close()

	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeErr)
		err := writeMultipart(multipartWriter, file, input)
		if err != nil {
			_ = writer.CloseWithError(err)
			writeErr <- err
			return
		}
		writeErr <- writer.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/audio/transcriptions", reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		return Result{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	response, requestErr := c.httpClient.Do(req)
	if requestErr != nil {
		_ = reader.CloseWithError(requestErr)
		<-writeErr
		return Result{}, fmt.Errorf("send transcription request: %w", requestErr)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseSize+1))
	// If Groq rejects a request before consuming its body, closing the pipe here
	// unblocks the multipart writer. For successful requests the upload has
	// already completed by the time response headers are returned.
	_ = reader.Close()
	streamErr := <-writeErr
	if err != nil {
		return Result{}, fmt.Errorf("read Groq response: %w", err)
	}
	if len(body) > maximumResponseSize {
		return Result{}, errors.New("Groq response exceeds 16 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{}, decodeAPIError(response.StatusCode, body, response.Header.Get("Retry-After"))
	}
	if streamErr != nil {
		return Result{}, fmt.Errorf("stream multipart request: %w", streamErr)
	}

	var decoded struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Result{}, fmt.Errorf("decode Groq response: %w", err)
	}
	return Result{Raw: body, Text: decoded.Text, Language: decoded.Language, Duration: decoded.Duration}, nil
}

func writeMultipart(writer *multipart.Writer, file *os.File, input Request) error {
	fields := [][2]string{
		{"model", input.Model},
		{"response_format", input.ResponseFormat},
		{"temperature", strconv.FormatFloat(input.Temperature, 'f', -1, 64)},
	}
	if input.Language != "" && input.Language != "auto" {
		fields = append(fields, [2]string{"language", input.Language})
	}
	if input.Prompt != "" {
		fields = append(fields, [2]string{"prompt", input.Prompt})
	}
	for _, field := range fields {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return err
		}
	}
	for _, granularity := range input.TimestampGranularities {
		if err := writer.WriteField("timestamp_granularities[]", granularity); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile("file", filepath.Base(input.FilePath))
	if err != nil {
		return err
	}
	_, err = io.Copy(part, file)
	if err != nil {
		return err
	}
	return writer.Close()
}

func decodeAPIError(status int, body []byte, retryAfter string) error {
	var decoded apiError
	message := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &decoded) == nil && decoded.Error.Message != "" {
		message = decoded.Error.Message
	}
	if len(message) > 1000 {
		message = message[:1000] + "…"
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return &APIError{StatusCode: status, Message: message, RetryAfter: retryAfter}
}

func PrettyJSON(raw []byte) ([]byte, error) {
	var output bytes.Buffer
	if err := json.Indent(&output, raw, "", "  "); err != nil {
		return nil, err
	}
	output.WriteByte('\n')
	return output.Bytes(), nil
}
