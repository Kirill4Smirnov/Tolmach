package speechmatics

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
	Diarization            string
	SpeakerSensitivity     float64
	PreferCurrentSpeaker   bool
	PunctuationSensitivity float64
	ExpectedLanguages      []string
	LanguageHints          []string
	AdditionalVocabulary   []string
	RemoveDisfluencies     bool
	WaitSeconds            int
}

type Result struct {
	JobID     string
	Raw       []byte
	Text      string
	Diarized  string
	Turns     []Turn
	Languages []string
}

type Turn struct {
	Speaker string
	Start   float64
	End     float64
	Text    string
}

type APIError struct {
	StatusCode int
	Message    string
	RetryAfter string
}

func (e *APIError) Error() string {
	if e.RetryAfter != "" {
		return fmt.Sprintf("Speechmatics API returned HTTP %d: %s (Retry-After: %s)", e.StatusCode, e.Message, e.RetryAfter)
	}
	return fmt.Sprintf("Speechmatics API returned HTTP %d: %s", e.StatusCode, e.Message)
}

type transcript struct {
	Format  string `json:"format"`
	Results []struct {
		Type         string  `json:"type"`
		StartTime    float64 `json:"start_time"`
		EndTime      float64 `json:"end_time"`
		Channel      string  `json:"channel"`
		Alternatives []struct {
			Content    string  `json:"content"`
			Language   string  `json:"language"`
			Speaker    string  `json:"speaker"`
			Confidence float64 `json:"confidence"`
		} `json:"alternatives"`
	} `json:"results"`
}

type createResponse struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	Transcript json.RawMessage `json:"json-v2"`
	Job        struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"job"`
}

func NewClient(apiKey string, options Options) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("empty Speechmatics API key")
	}
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://eu1.asr.api.speechmatics.com/v2"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Speechmatics base URL %q", baseURL)
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Minute}
	}
	return &Client{apiKey: apiKey, baseURL: baseURL, httpClient: httpClient}, nil
}

// Transcribe uploads a batch job, waits for completion, and returns json-v2.
// The input file is streamed and is never loaded completely into memory.
func (c *Client) Transcribe(ctx context.Context, input Request) (Result, error) {
	if err := validateRequest(input); err != nil {
		return Result{}, err
	}
	config, err := buildConfig(input)
	if err != nil {
		return Result{}, err
	}
	created, err := c.createJob(ctx, input.FilePath, config, input.WaitSeconds)
	if err != nil {
		return Result{}, err
	}
	jobID := created.ID
	if jobID == "" {
		jobID = created.Job.ID
	}
	if jobID == "" {
		return Result{}, errors.New("Speechmatics response contains no job ID")
	}
	status := created.Status
	if status == "" {
		status = created.Job.Status
	}
	if status == "rejected" || status == "deleted" {
		return Result{JobID: jobID}, fmt.Errorf("Speechmatics job %s ended with status %s%s", jobID, status, jobErrorSuffix(created))
	}

	var raw []byte
	if status == "done" && len(created.Transcript) > 0 && string(created.Transcript) != "null" {
		raw = append([]byte(nil), created.Transcript...)
	} else {
		if status != "done" {
			status, err = c.waitForJob(ctx, jobID, input.WaitSeconds)
			if err != nil {
				return Result{JobID: jobID}, err
			}
			if status != "done" {
				return Result{JobID: jobID}, fmt.Errorf("Speechmatics job %s ended with status %s", jobID, status)
			}
		}
		raw, err = c.getTranscript(ctx, jobID, input.WaitSeconds)
		if err != nil {
			return Result{JobID: jobID}, err
		}
	}
	parsed, err := ParseTranscript(raw)
	if err != nil {
		return Result{JobID: jobID, Raw: raw}, err
	}
	parsed.JobID = jobID
	return parsed, nil
}

func (c *Client) DeleteJob(ctx context.Context, jobID string) error {
	if strings.TrimSpace(jobID) == "" {
		return errors.New("empty Speechmatics job ID")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/jobs/"+url.PathEscape(jobID), nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete Speechmatics job: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response.StatusCode, body, response.Header.Get("Retry-After"))
	}
	return nil

}

func (c *Client) createJob(ctx context.Context, filePath string, config []byte, waitSeconds int) (createResponse, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return createResponse{}, fmt.Errorf("open input: %w", err)
	}
	defer file.Close()
	reader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeErr)
		err := writeMultipart(multipartWriter, file, filePath, config)
		if err != nil {
			_ = pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		writeErr <- pipeWriter.Close()
	}()
	endpoint := fmt.Sprintf("%s/jobs?wait=%d&format=json-v2", c.baseURL, waitSeconds)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		return createResponse{}, err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	response, requestErr := c.httpClient.Do(req)
	if requestErr != nil {
		_ = reader.CloseWithError(requestErr)
		<-writeErr
		return createResponse{}, fmt.Errorf("create Speechmatics job: %w", requestErr)
	}
	defer response.Body.Close()
	body, readErr := readLimited(response.Body)
	_ = reader.Close()
	streamErr := <-writeErr
	if readErr != nil {
		return createResponse{}, readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return createResponse{}, decodeAPIError(response.StatusCode, body, response.Header.Get("Retry-After"))
	}
	if streamErr != nil {
		return createResponse{}, fmt.Errorf("stream Speechmatics request: %w", streamErr)
	}
	var created createResponse
	if err := json.Unmarshal(body, &created); err != nil {
		return createResponse{}, fmt.Errorf("decode Speechmatics create response: %w", err)
	}
	return created, nil
}

func (c *Client) waitForJob(ctx context.Context, jobID string, waitSeconds int) (string, error) {
	for {
		endpoint := fmt.Sprintf("%s/jobs/%s?wait=%d", c.baseURL, url.PathEscape(jobID), waitSeconds)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", err
		}
		c.authorize(req)
		response, err := c.httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("get Speechmatics job status: %w", err)
		}
		body, readErr := readLimited(response.Body)
		response.Body.Close()
		if readErr != nil {
			return "", readErr
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return "", decodeAPIError(response.StatusCode, body, response.Header.Get("Retry-After"))
		}
		var details createResponse
		if err := json.Unmarshal(body, &details); err != nil {
			return "", fmt.Errorf("decode Speechmatics job status: %w", err)
		}
		status := details.Status
		if status == "" {
			status = details.Job.Status
		}
		switch status {
		case "done", "rejected", "deleted", "expired":
			return status, nil
		case "created", "running":
			continue
		default:
			if status == "" {
				return "", errors.New("Speechmatics job status response contains no status")
			}
			return status, nil
		}
	}
}

func (c *Client) getTranscript(ctx context.Context, jobID string, waitSeconds int) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/jobs/%s/transcript?wait=%d&format=json-v2", c.baseURL, url.PathEscape(jobID), waitSeconds)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get Speechmatics transcript: %w", err)
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeAPIError(response.StatusCode, body, response.Header.Get("Retry-After"))
	}
	return body, nil
}

func (c *Client) authorize(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
}

func writeMultipart(writer *multipart.Writer, file *os.File, filePath string, config []byte) error {
	if err := writer.WriteField("config", string(config)); err != nil {
		return err
	}
	part, err := writer.CreateFormFile("data_file", filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	return writer.Close()
}

func buildConfig(input Request) ([]byte, error) {
	type vocabulary struct {
		Content string `json:"content"`
	}
	type speakerConfig struct {
		PreferCurrentSpeaker bool    `json:"prefer_current_speaker"`
		SpeakerSensitivity   float64 `json:"speaker_sensitivity"`
	}
	type punctuationConfig struct {
		Sensitivity float64  `json:"sensitivity"`
		Permitted   []string `json:"permitted_marks"`
	}
	type filteringConfig struct {
		RemoveDisfluencies bool `json:"remove_disfluencies"`
	}
	type transcriptionConfig struct {
		Model               string            `json:"model"`
		Language            string            `json:"language"`
		LanguageHints       []string          `json:"language_hints,omitempty"`
		Diarization         string            `json:"diarization"`
		SpeakerConfig       *speakerConfig    `json:"speaker_diarization_config,omitempty"`
		Punctuation         punctuationConfig `json:"punctuation_overrides"`
		AdditionalVocab     []vocabulary      `json:"additional_vocab,omitempty"`
		TranscriptFiltering *filteringConfig  `json:"transcript_filtering_config,omitempty"`
	}
	type languageIDConfig struct {
		ExpectedLanguages []string `json:"expected_languages,omitempty"`
		LowConfidence     string   `json:"low_confidence_action"`
		DefaultLanguage   string   `json:"default_language,omitempty"`
	}
	payload := struct {
		Type          string              `json:"type"`
		Transcription transcriptionConfig `json:"transcription_config"`
		LanguageID    *languageIDConfig   `json:"language_identification_config,omitempty"`
	}{Type: "transcription"}
	payload.Transcription.Model = input.Model
	payload.Transcription.Language = input.Language
	payload.Transcription.LanguageHints = input.LanguageHints
	payload.Transcription.Diarization = input.Diarization
	payload.Transcription.Punctuation = punctuationConfig{Sensitivity: input.PunctuationSensitivity, Permitted: []string{"all"}}
	if input.Diarization == "speaker" {
		payload.Transcription.SpeakerConfig = &speakerConfig{
			PreferCurrentSpeaker: input.PreferCurrentSpeaker,
			SpeakerSensitivity:   input.SpeakerSensitivity,
		}
	}
	if input.RemoveDisfluencies {
		payload.Transcription.TranscriptFiltering = &filteringConfig{RemoveDisfluencies: true}
	}
	for _, word := range input.AdditionalVocabulary {
		if word = strings.TrimSpace(word); word != "" {
			payload.Transcription.AdditionalVocab = append(payload.Transcription.AdditionalVocab, vocabulary{Content: word})
		}
	}
	if input.Language == "auto" {
		payload.LanguageID = &languageIDConfig{ExpectedLanguages: input.ExpectedLanguages, LowConfidence: "allow"}
	}
	return json.Marshal(payload)
}

func validateRequest(input Request) error {
	if input.FilePath == "" {
		return errors.New("empty input file path")
	}
	if input.Model != "enhanced" && input.Model != "standard" && input.Model != "melia-1" {
		return fmt.Errorf("unsupported Speechmatics model %q", input.Model)
	}
	if input.Model == "melia-1" && input.Language != "multi" {
		return errors.New("Speechmatics melia-1 requires language=multi")
	}
	if input.Model != "melia-1" && input.Language == "multi" {
		return errors.New("Speechmatics language=multi requires model=melia-1")
	}
	if input.Language == "" {
		return errors.New("empty Speechmatics language")
	}
	if input.Diarization != "none" && input.Diarization != "speaker" && input.Diarization != "channel" {
		return fmt.Errorf("unsupported Speechmatics diarization %q", input.Diarization)
	}
	if input.SpeakerSensitivity < 0 || input.SpeakerSensitivity > 1 {
		return errors.New("speaker sensitivity must be between 0 and 1")
	}
	if input.PunctuationSensitivity < 0 || input.PunctuationSensitivity > 1 {
		return errors.New("punctuation sensitivity must be between 0 and 1")
	}
	if input.WaitSeconds < 1 || input.WaitSeconds > 120 {
		return errors.New("Speechmatics wait must be between 1 and 120 seconds")
	}
	if len(input.ExpectedLanguages) > 0 && input.Language != "auto" {
		return errors.New("expected languages require language=auto")
	}
	if len(input.LanguageHints) > 0 && input.Model != "melia-1" {
		return errors.New("language hints require model=melia-1")
	}
	if len(input.AdditionalVocabulary) > 0 && input.Model == "melia-1" {
		return errors.New("additional vocabulary is not supported by melia-1")
	}
	return nil
}

func ParseTranscript(raw []byte) (Result, error) {
	var decoded transcript
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Result{}, fmt.Errorf("decode Speechmatics transcript: %w", err)
	}
	var turns []Turn
	var full strings.Builder
	languages := make(map[string]bool)
	for _, item := range decoded.Results {
		if len(item.Alternatives) == 0 {
			continue
		}
		alternative := item.Alternatives[0]
		content := alternative.Content
		if content == "" {
			continue
		}
		if alternative.Language != "" {
			languages[alternative.Language] = true
		}
		speaker := alternative.Speaker
		if item.Channel != "" {
			speaker = item.Channel
		}
		// Punctuation normally carries a speaker label, but inheriting it makes
		// the renderer robust to older and synthetic json-v2 responses.
		if speaker == "" && item.Type == "punctuation" && len(turns) > 0 {
			speaker = turns[len(turns)-1].Speaker
		}
		if speaker == "" {
			speaker = "UU"
		}
		appendToken(&full, content, item.Type)
		if len(turns) == 0 || turns[len(turns)-1].Speaker != speaker {
			turns = append(turns, Turn{Speaker: speaker, Start: item.StartTime, End: item.EndTime})
		}
		turn := &turns[len(turns)-1]
		appendTokenToString(&turn.Text, content, item.Type)
		if item.EndTime > turn.End {
			turn.End = item.EndTime
		}
	}
	var languageList []string
	for language := range languages {
		languageList = append(languageList, language)
	}
	sort.Strings(languageList)
	diarized := FormatTurns(turns)
	return Result{Raw: raw, Text: strings.TrimSpace(full.String()), Diarized: diarized, Turns: turns, Languages: languageList}, nil
}

func FormatTurns(turns []Turn) string {
	var output strings.Builder
	for i, turn := range turns {
		if strings.TrimSpace(turn.Text) == "" {
			continue
		}
		if i > 0 {
			output.WriteString("\n\n")
		}
		fmt.Fprintf(&output, "[%s–%s] %s:\n%s", timestamp(turn.Start), timestamp(turn.End), turn.Speaker, strings.TrimSpace(turn.Text))
	}
	return output.String()
}

func appendToken(builder *strings.Builder, token, tokenType string) {
	text := builder.String()
	if needsSpace(text, token, tokenType) {
		builder.WriteByte(' ')
	}
	builder.WriteString(token)
}

func appendTokenToString(target *string, token, tokenType string) {
	if needsSpace(*target, token, tokenType) {
		*target += " "
	}
	*target += token
}

func needsSpace(existing, token, tokenType string) bool {
	if existing == "" || token == "" {
		return false
	}
	if tokenType == "punctuation" || strings.ContainsRune(".,!?;:%)]}»", []rune(token)[0]) {
		return false
	}
	last := []rune(existing)[len([]rune(existing))-1]
	return !strings.ContainsRune("([{«", last)
}

func timestamp(seconds float64) string {
	total := int(seconds + 0.5)
	if total >= 3600 {
		return fmt.Sprintf("%02d:%02d:%02d", total/3600, total/60%60, total%60)
	}
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

func readLimited(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximumResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read Speechmatics response: %w", err)
	}
	if len(body) > maximumResponseSize {
		return nil, errors.New("Speechmatics response exceeds 64 MiB")
	}
	return body, nil
}

func decodeAPIError(status int, body []byte, retryAfter string) error {
	message := strings.TrimSpace(string(body))
	var decoded struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(body, &decoded) == nil {
		for _, candidate := range []string{decoded.Message, decoded.Error, decoded.Detail} {
			if candidate != "" {
				message = candidate
				break
			}
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if len(message) > 1000 {
		message = message[:1000] + "…"
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

func jobErrorSuffix(response createResponse) string {
	if len(response.Job.Errors) == 0 || response.Job.Errors[0].Message == "" {
		return ""
	}
	return ": " + response.Job.Errors[0].Message
}
