package transcription

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"tolmach/internal/groq"
	"tolmach/internal/soniox"
	"tolmach/internal/speechmatics"
)

const (
	GroqModel         = "whisper-large-v3"
	SonioxModel       = "stt-async-v5"
	SpeechmaticsModel = "enhanced"
	TranslationModel  = "llama-3.3-70b-versatile"
)

const russianPrompt = "Это дословная расшифровка разговорной русской речи. Предложения оформлены с естественной русской пунктуацией: точки, запятые и вопросительные знаки."

type Service struct {
	Groq         *groq.Client
	Soniox       *soniox.Client
	Speechmatics *speechmatics.Client
}

type Request struct {
	FilePath    string
	Provider    string
	Language    string
	Diarization bool
}

type Result struct {
	Text      string
	Diarized  string
	Languages []string
	Duration  float64
	Warning   error
}

func Model(provider string) string {
	switch provider {
	case "groq":
		return GroqModel
	case "soniox":
		return SonioxModel
	case "speechmatics":
		return SpeechmaticsModel
	default:
		return "unknown"
	}
}

func (s *Service) Transcribe(ctx context.Context, input Request) (Result, error) {
	language := strings.ToLower(strings.TrimSpace(input.Language))
	if language == "auto" {
		language = ""
	}
	switch input.Provider {
	case "groq":
		if s.Groq == nil {
			return Result{}, errors.New("Groq is not configured")
		}
		if input.Diarization {
			return Result{}, errors.New("Groq does not support speaker diarization")
		}
		prompt := ""
		if language == "ru" {
			prompt = russianPrompt
		}
		result, err := s.Groq.Transcribe(ctx, groq.Request{
			FilePath: input.FilePath, Model: GroqModel, Language: language,
			Prompt: prompt, ResponseFormat: "verbose_json", Temperature: 0,
			TimestampGranularities: []string{"segment"},
		})
		if err != nil {
			return Result{}, err
		}
		languages := []string{}
		if result.Language != "" {
			languages = append(languages, result.Language)
		}
		return Result{Text: result.Text, Languages: languages, Duration: result.Duration}, nil
	case "soniox":
		if s.Soniox == nil {
			return Result{}, errors.New("Soniox is not configured")
		}
		hints := []string(nil)
		if language != "" {
			hints = []string{language}
		}
		result, err := s.Soniox.Transcribe(ctx, soniox.Request{
			FilePath: input.FilePath, Model: SonioxModel, LanguageHints: hints,
			EnableSpeakerDiarization: input.Diarization, EnableLanguageIdentification: true,
		})
		if result.FileID != "" || result.TranscriptionID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cleanupErr := s.Soniox.Cleanup(cleanupCtx, result.TranscriptionID, result.FileID)
			cancel()
			if err != nil && cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("clean up Soniox remote data: %w", cleanupErr))
			} else if cleanupErr != nil {
				return Result{Text: result.Text, Diarized: result.Diarized, Languages: result.Languages, Duration: result.Duration,
					Warning: fmt.Errorf("clean up Soniox remote data: %w", cleanupErr)}, nil
			}
		}
		if err != nil {
			return Result{}, err
		}
		return Result{Text: result.Text, Diarized: result.Diarized, Languages: result.Languages, Duration: result.Duration}, nil
	case "speechmatics":
		if s.Speechmatics == nil {
			return Result{}, errors.New("Speechmatics is not configured")
		}
		lang := language
		if lang == "" {
			lang = "auto"
		}
		diarization := "none"
		if input.Diarization {
			diarization = "speaker"
		}
		result, err := s.Speechmatics.Transcribe(ctx, speechmatics.Request{
			FilePath: input.FilePath, Model: SpeechmaticsModel, Language: lang, Diarization: diarization,
			SpeakerSensitivity: 0.5, PreferCurrentSpeaker: true, PunctuationSensitivity: 0.5, WaitSeconds: 60,
		})
		if result.JobID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cleanupErr := s.Speechmatics.DeleteJob(cleanupCtx, result.JobID)
			cancel()
			if err != nil && cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("clean up Speechmatics remote job: %w", cleanupErr))
			} else if cleanupErr != nil {
				return Result{Text: result.Text, Diarized: result.Diarized, Languages: result.Languages,
					Warning: fmt.Errorf("clean up Speechmatics remote job: %w", cleanupErr)}, nil
			}
		}
		if err != nil {
			return Result{}, err
		}
		return Result{Text: result.Text, Diarized: result.Diarized, Languages: result.Languages}, nil
	default:
		return Result{}, fmt.Errorf("unsupported provider %q", input.Provider)
	}
}

func (s *Service) Translate(ctx context.Context, text, target string) (string, error) {
	if s.Groq == nil {
		return "", errors.New("Groq is not configured")
	}
	return s.Groq.Translate(ctx, groq.TranslateRequest{Text: text, TargetLanguage: target, Model: TranslationModel})
}
