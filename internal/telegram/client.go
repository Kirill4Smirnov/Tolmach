package telegram

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
	"strconv"
	"strings"
	"time"
)

const maximumResponseSize = 16 << 20

type Client struct {
	token      string
	baseURL    string
	fileURL    string
	httpClient *http.Client
}

type Options struct {
	BaseURL    string
	FileURL    string
	HTTPClient *http.Client
}

type APIError struct {
	StatusCode  int
	ErrorCode   int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("Telegram API returned HTTP %d/code %d: %s (retry after %ds)", e.StatusCode, e.ErrorCode, e.Description, e.RetryAfter)
	}
	return fmt.Sprintf("Telegram API returned HTTP %d/code %d: %s", e.StatusCode, e.ErrorCode, e.Description)
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	MessageID      int      `json:"message_id"`
	From           *User    `json:"from"`
	Chat           Chat     `json:"chat"`
	Text           string   `json:"text"`
	Caption        string   `json:"caption"`
	ReplyToMessage *Message `json:"reply_to_message"`
	Voice          *File    `json:"voice"`
	VideoNote      *File    `json:"video_note"`
	Audio          *File    `json:"audio"`
	Video          *File    `json:"video"`
	Document       *File    `json:"document"`
}

type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	Duration     int    `json:"duration"`
	FilePath     string `json:"file_path"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type SendMessageRequest struct {
	ChatID                int64                 `json:"chat_id"`
	Text                  string                `json:"text"`
	ReplyToMessageID      int                   `json:"reply_to_message_id,omitempty"`
	DisableWebPagePreview bool                  `json:"disable_web_page_preview,omitempty"`
	ReplyMarkup           *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

func NewClient(token string, options Options) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("empty Telegram bot token")
	}
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	fileURL := strings.TrimRight(options.FileURL, "/")
	if fileURL == "" {
		fileURL = baseURL
	}
	for _, value := range []string{baseURL, fileURL} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("invalid Telegram base URL %q", value)
		}
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 70 * time.Second}
	}
	return &Client{token: token, baseURL: baseURL, fileURL: fileURL, httpClient: httpClient}, nil
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	if timeout < 0 || timeout > 50 {
		return nil, errors.New("Telegram polling timeout must be between 0 and 50")
	}
	var updates []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout, "allowed_updates": []string{"message", "callback_query"},
	}, &updates)
	return updates, err
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var user User
	err := c.call(ctx, "getMe", struct{}{}, &user)
	return user, err
}

func (c *Client) SendMessage(ctx context.Context, input SendMessageRequest) (Message, error) {
	var message Message
	err := c.call(ctx, "sendMessage", input, &message)
	return message, err
}

func (c *Client) EditMessage(ctx context.Context, chatID int64, messageID int, text string, markup *InlineKeyboardMarkup) error {
	var ignored json.RawMessage
	return c.call(ctx, "editMessageText", map[string]any{
		"chat_id": chatID, "message_id": messageID, "text": text, "reply_markup": markup,
	}, &ignored)
}

func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string, alert bool) error {
	var ignored bool
	return c.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID, "text": text, "show_alert": alert,
	}, &ignored)
}

func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	var file File
	err := c.call(ctx, "getFile", map[string]string{"file_id": fileID}, &file)
	return file, err
}

// SendDocument streams a local file to Telegram without loading it into RAM.
func (c *Client) SendDocument(ctx context.Context, chatID int64, localPath, fileName, caption string) (Message, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return Message{}, fmt.Errorf("open Telegram document: %w", err)
	}
	defer file.Close()
	reader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	writeErr := make(chan error, 1)
	go func() {
		err := multipartWriter.WriteField("chat_id", strconv.FormatInt(chatID, 10))
		if err == nil && caption != "" {
			err = multipartWriter.WriteField("caption", caption)
		}
		var part io.Writer
		if err == nil {
			part, err = multipartWriter.CreateFormFile("document", fileName)
		}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/sendDocument", reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		return Message{}, errors.New("create Telegram sendDocument request")
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	response, requestErr := c.httpClient.Do(req)
	if requestErr != nil {
		_ = reader.CloseWithError(requestErr)
		<-writeErr
		return Message{}, fmt.Errorf("call Telegram sendDocument: %w", sanitizeNetworkError(requestErr, c.token))
	}
	responseBody, readErr := readLimitedResponse(response)
	_ = reader.Close()
	streamErr := <-writeErr
	if readErr != nil {
		return Message{}, readErr
	}
	if streamErr != nil {
		return Message{}, fmt.Errorf("stream Telegram document: %w", streamErr)
	}
	var message Message
	if err := decodeEnvelope(response.StatusCode, responseBody, &message); err != nil {
		return Message{}, err
	}
	return message, nil
}

// Download streams a Telegram file to an already-created private local file.
// Error strings never include the request URL because it embeds the bot token.
func (c *Client) Download(ctx context.Context, remotePath, localPath string, maxBytes int64) error {
	if strings.TrimSpace(remotePath) == "" {
		return errors.New("empty Telegram file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileURL+"/file/bot"+c.token+"/"+strings.TrimLeft(remotePath, "/"), nil)
	if err != nil {
		return errors.New("create Telegram download request")
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download Telegram file: %w", sanitizeNetworkError(err, c.token))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("download Telegram file: HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(localPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary media: %w", err)
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		os.Remove(localPath)
		return fmt.Errorf("save Telegram file: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(localPath)
		return fmt.Errorf("close Telegram file: %w", closeErr)
	}
	if written > maxBytes {
		os.Remove(localPath)
		return fmt.Errorf("Telegram file exceeds limit of %d bytes", maxBytes)
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, payload, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Telegram %s request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Telegram %s request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call Telegram %s: %w", method, sanitizeNetworkError(err, c.token))
	}
	responseBody, err := readLimitedResponse(response)
	if err != nil {
		return fmt.Errorf("read Telegram %s response: %w", method, err)
	}
	return decodeEnvelope(response.StatusCode, responseBody, target)
}

func readLimitedResponse(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maximumResponseSize {
		return nil, errors.New("Telegram response exceeds 16 MiB")
	}
	return body, nil
}

func decodeEnvelope(statusCode int, responseBody []byte, target any) error {
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return fmt.Errorf("decode Telegram response: %w", err)
	}
	if !envelope.OK || statusCode < 200 || statusCode >= 300 {
		return &APIError{StatusCode: statusCode, ErrorCode: envelope.ErrorCode, Description: envelope.Description, RetryAfter: envelope.Parameters.RetryAfter}
	}
	if target != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, target); err != nil {
			return fmt.Errorf("decode Telegram result: %w", err)
		}
	}
	return nil
}

func sanitizeNetworkError(err error, token string) error {
	return errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
}

func SplitText(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{"(пустая транскрипция)"}
	}
	if limit <= 0 {
		limit = 4000
	}
	var chunks []string
	for len([]rune(text)) > limit {
		runes := []rune(text)
		cut := limit
		candidate := string(runes[:limit])
		if index := strings.LastIndex(candidate, "\n\n"); index > limit/2 {
			cut = len([]rune(candidate[:index]))
		} else if index := strings.LastIndex(candidate, "\n"); index > limit/2 {
			cut = len([]rune(candidate[:index]))
		} else if index := strings.LastIndex(candidate, " "); index > limit/2 {
			cut = len([]rune(candidate[:index]))
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[:cut])))
		text = strings.TrimSpace(string(runes[cut:]))
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

func FormatQueuePosition(position int) string {
	if position <= 0 {
		return "Начинаю обработку…"
	}
	return "Запись добавлена в очередь. Позиция: " + strconv.Itoa(position) + "."
}
