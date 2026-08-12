package soniox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTranscribeUploadsPollsAndParsesDiarization(t *testing.T) {
	input := filepath.Join(t.TempDir(), "dialog.ogg")
	if err := os.WriteFile(input, []byte("fake audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	polls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing authorization")
		}
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/files":
			mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
			if err != nil || mediaType != "multipart/form-data" {
				t.Fatalf("content type: %q, %v", mediaType, err)
			}
			reader := multipart.NewReader(request.Body, parameters["boundary"])
			part, err := reader.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(part)
			if err != nil || part.FormName() != "file" || part.FileName() != "dialog.ogg" || string(data) != "fake audio" {
				t.Fatalf("bad upload: field=%q file=%q data=%q err=%v", part.FormName(), part.FileName(), data, err)
			}
			return jsonResponse(request, http.StatusCreated, `{"id":"file-1"}`), nil
		case "POST /v1/transcriptions":
			var payload struct {
				Model                        string   `json:"model"`
				FileID                       string   `json:"file_id"`
				LanguageHints                []string `json:"language_hints"`
				LanguageHintsStrict          bool     `json:"language_hints_strict"`
				EnableSpeakerDiarization     bool     `json:"enable_speaker_diarization"`
				EnableLanguageIdentification bool     `json:"enable_language_identification"`
				Context                      struct {
					Text  string   `json:"text"`
					Terms []string `json:"terms"`
				} `json:"context"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Model != "stt-async-v5" || payload.FileID != "file-1" || !payload.EnableSpeakerDiarization || !payload.EnableLanguageIdentification || !payload.LanguageHintsStrict {
				t.Fatalf("unexpected config: %#v", payload)
			}
			if strings.Join(payload.LanguageHints, ",") != "ru,en" || payload.Context.Text != "A dialogue" || strings.Join(payload.Context.Terms, ",") != "Tolmach,Soniox" {
				t.Fatalf("unexpected hints/context: %#v", payload)
			}
			return jsonResponse(request, http.StatusCreated, `{"id":"tr-1","status":"queued"}`), nil
		case "GET /v1/transcriptions/tr-1":
			polls++
			if polls == 1 {
				return jsonResponse(request, http.StatusOK, `{"id":"tr-1","status":"processing"}`), nil
			}
			return jsonResponse(request, http.StatusOK, `{"id":"tr-1","status":"completed","audio_duration_ms":1800}`), nil
		case "GET /v1/transcriptions/tr-1/transcript":
			return jsonResponse(request, http.StatusOK, `{
  "id":"tr-1","text":"Привет, hello!","tokens":[
    {"text":"Привет","start_ms":100,"end_ms":500,"confidence":0.99,"speaker":"1","language":"ru"},
    {"text":", ","start_ms":500,"end_ms":550,"confidence":0.99,"speaker":"1","language":"ru"},
    {"text":"hello","start_ms":900,"end_ms":1400,"confidence":0.98,"speaker":"2","language":"en"},
    {"text":"!","start_ms":1400,"end_ms":1500,"confidence":0.98,"speaker":"2","language":"en"}
  ]}`), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
			return nil, nil
		}
	})
	client, err := NewClient("secret", Options{
		BaseURL: "https://soniox.test/", HTTPClient: &http.Client{Transport: transport}, PollInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Transcribe(context.Background(), Request{
		FilePath: input, Model: "stt-async-v5", LanguageHints: []string{"ru", "en"}, LanguageHintsStrict: true,
		EnableSpeakerDiarization: true, EnableLanguageIdentification: true,
		Context: "A dialogue", Terms: []string{"Tolmach", "Soniox"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FileID != "file-1" || result.TranscriptionID != "tr-1" || result.Duration != 1.8 || result.Text != "Привет, hello!" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Diarized != "[00:00–00:01] Speaker 1:\nПривет,\n\n[00:01–00:02] Speaker 2:\nhello!" {
		t.Fatalf("diarized:\n%s", result.Diarized)
	}
	if strings.Join(result.Languages, ",") != "en,ru" {
		t.Fatalf("languages = %#v", result.Languages)
	}
	wantCalls := "POST /v1/files,POST /v1/transcriptions,GET /v1/transcriptions/tr-1,GET /v1/transcriptions/tr-1,GET /v1/transcriptions/tr-1/transcript"
	if strings.Join(calls, ",") != wantCalls {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestCleanupOrderAndPartialFailureIDs(t *testing.T) {
	input := filepath.Join(t.TempDir(), "talk.mp4")
	if err := os.WriteFile(input, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	var deletes []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Body != nil {
			_, _ = io.Copy(io.Discard, request.Body)
		}
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/files":
			return jsonResponse(request, http.StatusCreated, `{"id":"file-2"}`), nil
		case "POST /v1/transcriptions":
			return jsonResponse(request, http.StatusCreated, `{"id":"tr-2","status":"error","error_type":"invalid_audio","error_message":"cannot decode"}`), nil
		case "DELETE /v1/transcriptions/tr-2", "DELETE /v1/files/file-2":
			deletes = append(deletes, request.URL.Path)
			return jsonResponse(request, http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
			return nil, nil
		}
	})
	client, _ := NewClient("secret", Options{BaseURL: "https://soniox.test", HTTPClient: &http.Client{Transport: transport}, PollInterval: time.Nanosecond})
	result, err := client.Transcribe(context.Background(), Request{FilePath: input, Model: "stt-async-v5"})
	if err == nil || !strings.Contains(err.Error(), "invalid_audio") {
		t.Fatalf("error = %v", err)
	}
	if result.FileID != "file-2" || result.TranscriptionID != "tr-2" {
		t.Fatalf("partial IDs lost: %#v", result)
	}
	if err := client.Cleanup(context.Background(), result.TranscriptionID, result.FileID); err != nil {
		t.Fatal(err)
	}
	if strings.Join(deletes, ",") != "/v1/transcriptions/tr-2,/v1/files/file-2" {
		t.Fatalf("delete order = %#v", deletes)
	}
}

func TestAPIErrorDoesNotLeakKey(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, http.StatusUnauthorized, `{"error_type":"authentication_error","message":"bad credentials","request_id":"req-1"}`), nil
	})
	client, _ := NewClient("do-not-leak", Options{BaseURL: "https://soniox.test", HTTPClient: &http.Client{Transport: transport}})
	err := client.deleteResource(context.Background(), "/v1/files/file-3")
	if err == nil || !strings.Contains(err.Error(), "bad credentials") || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("unsafe or unhelpful error: %v", err)
	}
}

func jsonResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    request,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
