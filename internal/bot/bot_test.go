package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tolmach/internal/store"
	"tolmach/internal/telegram"
	"tolmach/internal/transcription"
)

type fakeEngine struct {
	request transcription.Request
}

func (f *fakeEngine) Transcribe(_ context.Context, input transcription.Request) (transcription.Result, error) {
	f.request = input
	data, err := os.ReadFile(input.FilePath)
	if err != nil || string(data) != "audio" {
		return transcription.Result{}, errors.New("bad local media")
	}
	return transcription.Result{Text: "Привет.", Languages: []string{"ru"}, Duration: 1}, nil
}

func (f *fakeEngine) Translate(context.Context, string, string) (string, error) {
	return "Hello.", nil
}

func TestVoiceMessageIsQueuedProcessedAndCached(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var finalEdit map[string]any
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.Method + " " + request.URL.Path {
		case "POST /botsecret/sendMessage":
			return jsonResponse(request, http.StatusOK, `{"ok":true,"result":{"message_id":100,"chat":{"id":7,"type":"private"}}}`), nil
		case "POST /botsecret/getFile":
			return jsonResponse(request, http.StatusOK, `{"ok":true,"result":{"file_id":"file-1","file_path":"voice/a.ogg"}}`), nil
		case "GET /file/botsecret/voice/a.ogg":
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString("audio")), Request: request}, nil
		case "POST /botsecret/editMessageText":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(payload["text"].(string), "Привет") {
				finalEdit = payload
			}
			return jsonResponse(request, http.StatusOK, `{"ok":true,"result":true}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})
	client, _ := telegram.NewClient("secret", telegram.Options{BaseURL: "https://telegram.test", HTTPClient: &http.Client{Transport: transport}})
	engine := &fakeEngine{}
	application, err := New(client, database, engine, Config{AllowedUsers: map[int64]bool{42: true}, TempDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	message := &telegram.Message{MessageID: 9, From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: 7, Type: "private"},
		Voice: &telegram.File{FileID: "file-1", FileUniqueID: "unique-1", FileSize: 5}}
	if err := application.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, Message: message}); err != nil {
		t.Fatal(err)
	}
	jobID := <-application.queue
	application.processJob(context.Background(), jobID)
	if engine.request.Provider != "groq" || engine.request.Language != "ru" || engine.request.Diarization {
		t.Fatalf("request = %#v", engine.request)
	}
	job, err := database.Job(context.Background(), jobID)
	if err != nil || job.Status != "completed" || job.Text != "Привет." || job.ProcessingMilliseconds < 0 {
		t.Fatalf("job=%#v err=%v", job, err)
	}
	if finalEdit == nil || finalEdit["reply_markup"] == nil {
		t.Fatalf("final edit = %#v", finalEdit)
	}
	if _, found, err := database.CachedJob(context.Background(), "unique-1", "groq", transcription.GroqModel, "ru", false); err != nil || !found {
		t.Fatalf("cache found=%v err=%v", found, err)
	}
}

func TestLanguageValidationAndCommandParsing(t *testing.T) {
	for _, language := range []string{"ru", "eng", "auto"} {
		if !validLanguage(language) {
			t.Fatalf("expected %q to be valid", language)
		}
	}
	for _, language := range []string{"r", "ru-RU", "рус"} {
		if validLanguage(language) {
			t.Fatalf("expected %q to be invalid", language)
		}
	}
	command, argument := parseCommand("/translate@tolmach_voice_bot en extra")
	if command != "translate" || argument != "en" {
		t.Fatalf("command=%q argument=%q", command, argument)
	}
}

func TestMediaExtensionNormalizesTelegramVoiceOGA(t *testing.T) {
	job := store.Job{MediaKind: "voice"}
	if got := mediaExtension(job, "voice/file_12.oga"); got != ".ogg" {
		t.Fatalf("extension = %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func jsonResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body)), Request: request}
}
