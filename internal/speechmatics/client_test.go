package speechmatics

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
)

func TestTranscribeEmbeddedTranscriptAndConfig(t *testing.T) {
	input := filepath.Join(t.TempDir(), "talk.ogg")
	if err := os.WriteFile(input, []byte("fake audio"), 0o600); err != nil {
		t.Fatal(err)
	}

	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodPost || request.URL.Path != "/v2/jobs" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
		}
		if request.URL.Query().Get("wait") != "60" || request.URL.Query().Get("format") != "json-v2" {
			t.Fatalf("unexpected query: %s", request.URL.RawQuery)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing authorization")
		}
		mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("content type: %q, %v", mediaType, err)
		}
		reader := multipart.NewReader(request.Body, parameters["boundary"])
		fields := readMultipart(t, reader)
		if string(fields["data_file"]) != "fake audio" {
			t.Fatalf("uploaded data = %q", fields["data_file"])
		}
		var config map[string]any
		if err := json.Unmarshal(fields["config"], &config); err != nil {
			t.Fatal(err)
		}
		transcription := config["transcription_config"].(map[string]any)
		if transcription["model"] != "enhanced" || transcription["language"] != "auto" || transcription["diarization"] != "speaker" {
			t.Fatalf("unexpected transcription config: %#v", transcription)
		}
		languageID := config["language_identification_config"].(map[string]any)
		if got := languageID["expected_languages"].([]any); len(got) != 2 || got[0] != "ru" || got[1] != "en" {
			t.Fatalf("expected languages = %#v", got)
		}
		return jsonResponse(request, http.StatusCreated, `{
  "id":"job-1","status":"done","json-v2":{"format":"2.9","results":[
    {"type":"word","start_time":0.1,"end_time":0.4,"alternatives":[{"content":"Привет","language":"ru","speaker":"S1"}]},
    {"type":"punctuation","start_time":0.4,"end_time":0.4,"alternatives":[{"content":",","language":"ru","speaker":"S1"}]},
    {"type":"word","start_time":0.5,"end_time":0.8,"alternatives":[{"content":"hello","language":"en","speaker":"S2"}]},
    {"type":"punctuation","start_time":0.8,"end_time":0.8,"alternatives":[{"content":"!","language":"en","speaker":"S2"}]}
  ]}}
`), nil
	})
	client, err := NewClient("secret", Options{
		BaseURL:    "https://speech.test/v2/",
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Transcribe(context.Background(), Request{
		FilePath: input, Model: "enhanced", Language: "auto", Diarization: "speaker",
		SpeakerSensitivity: 0.5, PreferCurrentSpeaker: true, PunctuationSensitivity: 0.5,
		ExpectedLanguages: []string{"ru", "en"}, WaitSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || result.JobID != "job-1" || result.Text != "Привет, hello!" {
		t.Fatalf("unexpected result: requests=%d result=%#v", requests, result)
	}
	if result.Diarized != "[00:00–00:00] S1:\nПривет,\n\n[00:01–00:01] S2:\nhello!" {
		t.Fatalf("diarized:\n%s", result.Diarized)
	}
	if strings.Join(result.Languages, ",") != "en,ru" {
		t.Fatalf("languages = %#v", result.Languages)
	}
}

func TestTranscribePollsAndFetchesTranscript(t *testing.T) {
	input := filepath.Join(t.TempDir(), "talk.mp4")
	if err := os.WriteFile(input, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	var paths []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.Method+" "+request.URL.Path)
		if request.Body != nil {
			_, _ = io.Copy(io.Discard, request.Body)
		}
		switch request.Method + " " + request.URL.Path {
		case "POST /v2/jobs":
			return jsonResponse(request, http.StatusCreated, `{"id":"job-2","status":"created"}`), nil
		case "GET /v2/jobs/job-2":
			return jsonResponse(request, http.StatusOK, `{"job":{"id":"job-2","status":"done"}}`), nil
		case "GET /v2/jobs/job-2/transcript":
			return jsonResponse(request, http.StatusOK, `{"format":"2.9","results":[]}`), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
			return nil, nil
		}
	})
	client, _ := NewClient("secret", Options{BaseURL: "https://speech.test/v2", HTTPClient: &http.Client{Transport: transport}})
	result, err := client.Transcribe(context.Background(), Request{
		FilePath: input, Model: "standard", Language: "ru", Diarization: "none",
		SpeakerSensitivity: 0.5, PunctuationSensitivity: 0.5, WaitSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.JobID != "job-2" || strings.Join(paths, ",") != "POST /v2/jobs,GET /v2/jobs/job-2,GET /v2/jobs/job-2/transcript" {
		t.Fatalf("result=%#v paths=%#v", result, paths)
	}
}

func TestDeleteJobAndAPIError(t *testing.T) {
	var deleted string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			deleted = request.URL.Path
			return jsonResponse(request, http.StatusNoContent, ""), nil
		}
		return jsonResponse(request, http.StatusUnauthorized, `{"detail":"bad credentials"}`), nil
	})
	client, _ := NewClient("do-not-leak", Options{BaseURL: "https://speech.test/v2", HTTPClient: &http.Client{Transport: transport}})
	if err := client.DeleteJob(context.Background(), "job with space"); err != nil {
		t.Fatal(err)
	}
	if deleted != "/v2/jobs/job with space" {
		t.Fatalf("deleted path = %q", deleted)
	}
	err := decodeAPIError(http.StatusUnauthorized, []byte(`{"detail":"bad credentials"}`), "3")
	if !strings.Contains(err.Error(), "bad credentials") || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("unsafe or unhelpful error: %v", err)
	}
}

func TestValidateMeliaAndFeatureCombinations(t *testing.T) {
	base := Request{FilePath: "x", Model: "melia-1", Language: "multi", Diarization: "speaker", SpeakerSensitivity: 0.5, PunctuationSensitivity: 0.5, WaitSeconds: 60}
	if err := validateRequest(base); err != nil {
		t.Fatal(err)
	}
	bad := base
	bad.Language = "auto"
	if err := validateRequest(bad); err == nil {
		t.Fatal("expected melia language validation error")
	}
	bad = base
	bad.AdditionalVocabulary = []string{"Tolmach"}
	if err := validateRequest(bad); err == nil {
		t.Fatal("expected melia vocabulary validation error")
	}
}

func readMultipart(t *testing.T, reader *multipart.Reader) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		result[part.FormName()] = data
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
