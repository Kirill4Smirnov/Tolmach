package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tolmach/internal/config"
	"tolmach/internal/groq"
	"tolmach/internal/output"
	"tolmach/internal/soniox"
	"tolmach/internal/speechmatics"
)

const defaultMaxMiB int64 = 25

const defaultRussianPrompt = "Это дословная расшифровка разговорной русской речи. Предложения оформлены с естественной русской пунктуацией: точки, запятые и вопросительные знаки."

type options struct {
	provider                string
	model                   string
	language                string
	prompt                  string
	format                  string
	timestamps              string
	outputDir               string
	dotEnv                  string
	baseURL                 string
	timeout                 time.Duration
	maxMiB                  int64
	temperature             float64
	overwrite               bool
	printText               bool
	dryRun                  bool
	polish                  bool
	polishModel             string
	speechmaticsModel       string
	diarization             string
	speakerSensitivity      float64
	preferCurrentSpeaker    bool
	punctuationSensitivity  float64
	expectedLanguages       string
	languageHints           string
	vocabulary              string
	removeDisfluencies      bool
	speechmaticsWaitSeconds int
	keepRemoteJob           bool
	sonioxModel             string
	sonioxContext           string
	sonioxLanguageStrict    bool
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	var opts options
	flags := flag.NewFlagSet("transcribe", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&opts.provider, "provider", "groq", "transcription provider: groq, speechmatics, or soniox")
	flags.StringVar(&opts.model, "model", "whisper-large-v3", "Groq transcription model")
	flags.StringVar(&opts.language, "language", "ru", "language code, auto, or multi for Melia 1")
	flags.StringVar(&opts.prompt, "prompt", defaultRussianPrompt, "vocabulary/context/style hint; pass an empty value to disable")
	flags.StringVar(&opts.format, "format", "verbose_json", "response format: json or verbose_json")
	flags.StringVar(&opts.timestamps, "timestamps", "segment", "comma-separated: segment,word; empty disables")
	flags.StringVar(&opts.outputDir, "out", "transcripts", "output directory")
	flags.StringVar(&opts.dotEnv, "env-file", ".env", "dotenv file (existing environment wins)")
	flags.StringVar(&opts.baseURL, "base-url", "", "override API base URL for the selected provider")
	flags.DurationVar(&opts.timeout, "timeout", 10*time.Minute, "timeout per file")
	flags.Int64Var(&opts.maxMiB, "max-mib", defaultMaxMiB, "reject files larger than this; 0 disables")
	flags.Float64Var(&opts.temperature, "temperature", 0, "sampling temperature")
	flags.BoolVar(&opts.overwrite, "overwrite", false, "overwrite existing result files")
	flags.BoolVar(&opts.printText, "print", false, "also print transcript to stdout")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "validate inputs and print planned outputs without API calls")
	flags.BoolVar(&opts.polish, "polish", false, "restore punctuation in a separate, lexically verified output")
	flags.StringVar(&opts.polishModel, "polish-model", "llama-3.3-70b-versatile", "Groq text model used by --polish")
	flags.StringVar(&opts.speechmaticsModel, "speechmatics-model", "enhanced", "Speechmatics model: enhanced, standard, or melia-1")
	flags.StringVar(&opts.diarization, "diarization", "speaker", "Speechmatics diarization: speaker, channel, or none")
	flags.Float64Var(&opts.speakerSensitivity, "speaker-sensitivity", 0.5, "Speechmatics speaker sensitivity from 0 to 1")
	flags.BoolVar(&opts.preferCurrentSpeaker, "prefer-current-speaker", true, "reduce false speaker switches in Speechmatics")
	flags.Float64Var(&opts.punctuationSensitivity, "punctuation-sensitivity", 0.5, "Speechmatics punctuation sensitivity from 0 to 1")
	flags.StringVar(&opts.expectedLanguages, "expected-languages", "", "comma-separated candidates used with Speechmatics language=auto")
	flags.StringVar(&opts.languageHints, "language-hints", "", "comma-separated language hints for Speechmatics Melia 1 or Soniox")
	flags.StringVar(&opts.vocabulary, "vocab", "", "comma-separated custom vocabulary for Speechmatics or Soniox")
	flags.BoolVar(&opts.removeDisfluencies, "remove-disfluencies", false, "remove supported filler words in Speechmatics")
	flags.IntVar(&opts.speechmaticsWaitSeconds, "speechmatics-wait", 60, "seconds per synchronous Speechmatics wait request (1-120)")
	flags.BoolVar(&opts.keepRemoteJob, "keep-remote-job", false, "do not delete remote Speechmatics/Soniox data")
	flags.StringVar(&opts.sonioxModel, "soniox-model", "stt-async-v5", "Soniox async transcription model")
	flags.StringVar(&opts.sonioxContext, "soniox-context", "", "free-form Soniox context describing names, topic, and expected content")
	flags.BoolVar(&opts.sonioxLanguageStrict, "soniox-language-hints-strict", false, "restrict Soniox recognition to its language hints")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: transcribe [flags] FILE [FILE...]")
		fmt.Fprintln(flags.Output(), "\nStreams local media to Groq, Speechmatics, or Soniox and writes private transcript files.")
		fmt.Fprintln(flags.Output(), "\nFlags:")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	files := flags.Args()
	if len(files) == 0 {
		flags.Usage()
		return 2
	}
	if err := validateOptions(opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if err := config.LoadDotEnv(opts.dotEnv); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	granularities, err := parseGranularities(opts.timestamps)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	for _, path := range files {
		if err := validateInput(path, opts.maxMiB); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			return 1
		}
		paths := resultPaths(opts, path)
		if opts.dryRun {
			info, _ := os.Stat(path)
			if (opts.provider == "speechmatics" || opts.provider == "soniox") && opts.diarization != "none" {
				fmt.Printf("%s (%s) -> %s, %s, %s\n", path, humanBytes(info.Size()), paths.Text, paths.Speakers, paths.JSON)
			} else {
				fmt.Printf("%s (%s) -> %s, %s\n", path, humanBytes(info.Size()), paths.Text, paths.JSON)
			}
		}
	}
	if opts.dryRun {
		return 0
	}

	rootContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if opts.provider == "speechmatics" {
		return runSpeechmatics(rootContext, files, opts)
	}
	if opts.provider == "soniox" {
		return runSoniox(rootContext, files, opts)
	}
	client, err := groq.NewClient(os.Getenv("GROQ_API_KEY"), groq.Options{BaseURL: opts.baseURL})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: GROQ_API_KEY is missing or invalid; set it in the environment or .env")
		return 1
	}
	failures := 0
	abortBatch := false
	for index, path := range files {
		if rootContext.Err() != nil {
			failures += len(files) - index
			break
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] Transcribing %s with %s…\n", index+1, len(files), filepath.Base(path), opts.model)
		ctx, cancel := context.WithTimeout(rootContext, opts.timeout)
		started := time.Now()
		result, err := client.Transcribe(ctx, groq.Request{
			FilePath: path, Model: opts.model, Language: normalizedLanguage(opts.language),
			Prompt: opts.prompt, ResponseFormat: opts.format, Temperature: opts.temperature,
			TimestampGranularities: granularities,
		})
		cancel()
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			var apiErr *groq.APIError
			if errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
				remaining := len(files) - index - 1
				if remaining > 0 {
					failures += remaining
					fmt.Fprintf(os.Stderr, "Authentication or network access was rejected; skipping %d remaining file(s).\n", remaining)
				}
				abortBatch = true
			}
			if abortBatch {
				break
			}
			continue
		}
		prettyJSON, err := groq.PrettyJSON(result.Raw)
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "%s: format JSON: %v\n", path, err)
			continue
		}
		paths := resultPaths(opts, path)
		if err := output.WriteResults(paths, result.Text, prettyJSON, opts.overwrite); err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			continue
		}
		written := paths.Text + " and " + paths.JSON
		if opts.polish {
			polishContext, polishCancel := context.WithTimeout(rootContext, opts.timeout)
			polished, polishErr := client.PolishPunctuation(polishContext, groq.PolishRequest{Text: result.Text, Model: opts.polishModel})
			polishCancel()
			if polishErr != nil {
				fmt.Fprintf(os.Stderr, "    Warning: punctuation pass skipped: %v\n", polishErr)
			} else {
				polishedPath := output.PolishedPath(paths)
				if err := output.WriteText(polishedPath, polished, opts.overwrite); err != nil {
					fmt.Fprintf(os.Stderr, "    Warning: could not write polished text: %v\n", err)
				} else {
					written += ", and " + polishedPath
				}
			}
		}
		fmt.Fprintf(os.Stderr, "    Done in %s; audio %.1fs; wrote %s\n", time.Since(started).Round(time.Millisecond), result.Duration, written)
		if opts.printText {
			fmt.Printf("\n=== %s ===\n%s\n", filepath.Base(path), result.Text)
		}
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "Completed with %d failed file(s).\n", failures)
		return 1
	}
	return 0
}

func validateOptions(opts options) error {
	if opts.provider != "groq" && opts.provider != "speechmatics" && opts.provider != "soniox" {
		return errors.New("--provider must be groq, speechmatics, or soniox")
	}
	if opts.model == "" {
		return errors.New("--model cannot be empty")
	}
	if opts.provider == "speechmatics" {
		if opts.polish {
			return errors.New("--polish is currently available only with --provider groq")
		}
		if opts.speechmaticsModel != "enhanced" && opts.speechmaticsModel != "standard" && opts.speechmaticsModel != "melia-1" {
			return errors.New("--speechmatics-model must be enhanced, standard, or melia-1")
		}
		if opts.diarization != "speaker" && opts.diarization != "channel" && opts.diarization != "none" {
			return errors.New("--diarization must be speaker, channel, or none")
		}
		language := strings.ToLower(strings.TrimSpace(opts.language))
		if opts.speechmaticsModel == "melia-1" && language != "multi" {
			return errors.New("Speechmatics melia-1 requires --language multi")
		}
		if opts.speechmaticsModel != "melia-1" && language == "multi" {
			return errors.New("Speechmatics --language multi requires --speechmatics-model melia-1")
		}
		if strings.TrimSpace(opts.expectedLanguages) != "" && language != "auto" {
			return errors.New("--expected-languages requires --language auto")
		}
		if strings.TrimSpace(opts.languageHints) != "" && opts.speechmaticsModel != "melia-1" {
			return errors.New("--language-hints requires --speechmatics-model melia-1")
		}
		if strings.TrimSpace(opts.vocabulary) != "" && opts.speechmaticsModel == "melia-1" {
			return errors.New("--vocab is not supported by Speechmatics melia-1")
		}
		if opts.speakerSensitivity < 0 || opts.speakerSensitivity > 1 {
			return errors.New("--speaker-sensitivity must be between 0 and 1")
		}
		if opts.punctuationSensitivity < 0 || opts.punctuationSensitivity > 1 {
			return errors.New("--punctuation-sensitivity must be between 0 and 1")
		}
		if opts.speechmaticsWaitSeconds < 1 || opts.speechmaticsWaitSeconds > 120 {
			return errors.New("--speechmatics-wait must be between 1 and 120")
		}
	}
	if opts.provider == "soniox" {
		if opts.polish {
			return errors.New("--polish is currently available only with --provider groq")
		}
		if strings.TrimSpace(opts.sonioxModel) == "" {
			return errors.New("--soniox-model cannot be empty")
		}
		if opts.diarization != "speaker" && opts.diarization != "none" {
			return errors.New("Soniox --diarization must be speaker or none")
		}
		language := strings.ToLower(strings.TrimSpace(opts.language))
		if language == "multi" {
			return errors.New("Soniox does not use --language multi; use auto or language hints")
		}
		if strings.TrimSpace(opts.expectedLanguages) != "" {
			return errors.New("--expected-languages is specific to Speechmatics; use --language-hints with Soniox")
		}
		if opts.sonioxLanguageStrict && language == "auto" && len(commaSeparated(opts.languageHints)) == 0 {
			return errors.New("--soniox-language-hints-strict requires --language or --language-hints")
		}
	}
	if opts.polish && strings.TrimSpace(opts.polishModel) == "" {
		return errors.New("--polish-model cannot be empty when --polish is enabled")
	}
	if opts.format != "json" && opts.format != "verbose_json" {
		return errors.New("--format must be json or verbose_json")
	}
	if opts.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if opts.maxMiB < 0 {
		return errors.New("--max-mib cannot be negative")
	}
	if opts.temperature < 0 || opts.temperature > 1 {
		return errors.New("--temperature must be between 0 and 1")
	}
	return nil
}

func validateInput(path string, maxMiB int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if info.Size() == 0 {
		return errors.New("file is empty")
	}
	if maxMiB > 0 && info.Size() > maxMiB*1024*1024 {
		return fmt.Errorf("file is %s, exceeding --max-mib=%d", humanBytes(info.Size()), maxMiB)
	}
	return nil
}

func parseGranularities(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	seen := make(map[string]bool)
	var result []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "segment" && item != "word" {
			return nil, fmt.Errorf("unsupported timestamp granularity %q", item)
		}
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result, nil
}

func normalizedLanguage(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "auto") {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func speechmaticsLanguage(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func commaSeparated(value string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func resultPaths(opts options, inputPath string) output.Paths {
	if opts.provider == "speechmatics" {
		model := "speechmatics-" + opts.speechmaticsModel + "-" + opts.diarization
		return output.ResultPaths(opts.outputDir, inputPath, model, speechmaticsLanguage(opts.language))
	}
	if opts.provider == "soniox" {
		model := "soniox-" + opts.sonioxModel + "-" + opts.diarization
		return output.ResultPaths(opts.outputDir, inputPath, model, normalizedLanguage(opts.language))
	}
	return output.ResultPaths(opts.outputDir, inputPath, opts.model, normalizedLanguage(opts.language))
}

func runSoniox(rootContext context.Context, files []string, opts options) int {
	client, err := soniox.NewClient(os.Getenv("SONIOX_API_KEY"), soniox.Options{BaseURL: opts.baseURL})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: SONIOX_API_KEY is missing or invalid; set it in the environment or .env")
		return 1
	}
	failures := 0
	for index, path := range files {
		if rootContext.Err() != nil {
			failures += len(files) - index
			break
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] Transcribing %s with Soniox %s (%s diarization)…\n",
			index+1, len(files), filepath.Base(path), opts.sonioxModel, opts.diarization)
		ctx, cancel := context.WithTimeout(rootContext, opts.timeout)
		started := time.Now()
		hints := commaSeparated(opts.languageHints)
		if language := normalizedLanguage(opts.language); language != "" {
			hints = append([]string{language}, hints...)
			hints = uniqueStrings(hints)
		}
		result, transcribeErr := client.Transcribe(ctx, soniox.Request{
			FilePath: path, Model: opts.sonioxModel, LanguageHints: hints,
			LanguageHintsStrict:          opts.sonioxLanguageStrict,
			EnableSpeakerDiarization:     opts.diarization == "speaker",
			EnableLanguageIdentification: true,
			Context:                      opts.sonioxContext, Terms: commaSeparated(opts.vocabulary),
		})
		cancel()
		if transcribeErr != nil {
			if !opts.keepRemoteJob {
				cleanupSoniox(rootContext, client, result.TranscriptionID, result.FileID)
			}
			failures++
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, transcribeErr)
			var apiErr *soniox.APIError
			if errors.As(transcribeErr, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
				remaining := len(files) - index - 1
				failures += remaining
				if remaining > 0 {
					fmt.Fprintf(os.Stderr, "Soniox authentication was rejected; skipping %d remaining file(s).\n", remaining)
				}
				break
			}
			continue
		}
		prettyJSON, prettyErr := soniox.PrettyJSON(result.Raw)
		if prettyErr != nil {
			if !opts.keepRemoteJob {
				cleanupSoniox(rootContext, client, result.TranscriptionID, result.FileID)
			}
			failures++
			fmt.Fprintf(os.Stderr, "%s: format JSON: %v\n", path, prettyErr)
			continue
		}
		paths := resultPaths(opts, path)
		written := paths.Text + " and " + paths.JSON
		var writeErr error
		if opts.diarization == "speaker" {
			writeErr = output.WriteDiarizedResults(paths, result.Text, result.Diarized, prettyJSON, opts.overwrite)
			written = paths.Text + ", " + paths.Speakers + ", and " + paths.JSON
		} else {
			writeErr = output.WriteResults(paths, result.Text, prettyJSON, opts.overwrite)
		}
		if !opts.keepRemoteJob {
			cleanupSoniox(rootContext, client, result.TranscriptionID, result.FileID)
		}
		if writeErr != nil {
			failures++
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, writeErr)
			continue
		}
		languageInfo := ""
		if len(result.Languages) > 0 {
			languageInfo = "; language(s) " + strings.Join(result.Languages, ",")
		}
		fmt.Fprintf(os.Stderr, "    Done in %s; audio %.1fs%s; wrote %s\n",
			time.Since(started).Round(time.Millisecond), result.Duration, languageInfo, written)
		if opts.printText {
			text := result.Text
			if opts.diarization == "speaker" {
				text = result.Diarized
			}
			fmt.Printf("\n=== %s ===\n%s\n", filepath.Base(path), text)
		}
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "Completed with %d failed file(s).\n", failures)
		return 1
	}
	return 0
}

func cleanupSoniox(parent context.Context, client *soniox.Client, transcriptionID, fileID string) {
	cleanupContext, cleanupCancel := context.WithTimeout(parent, 30*time.Second)
	cleanupErr := client.Cleanup(cleanupContext, transcriptionID, fileID)
	cleanupCancel()
	if cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "    Warning: remote Soniox data could not be fully deleted: %v\n", cleanupErr)
	}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func runSpeechmatics(rootContext context.Context, files []string, opts options) int {
	client, err := speechmatics.NewClient(os.Getenv("SPEECHMATICS_API_KEY"), speechmatics.Options{BaseURL: opts.baseURL})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: SPEECHMATICS_API_KEY is missing or invalid; set it in the environment or .env")
		return 1
	}
	failures := 0
	for index, path := range files {
		if rootContext.Err() != nil {
			failures += len(files) - index
			break
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] Transcribing %s with Speechmatics %s (%s diarization)…\n",
			index+1, len(files), filepath.Base(path), opts.speechmaticsModel, opts.diarization)
		ctx, cancel := context.WithTimeout(rootContext, opts.timeout)
		started := time.Now()
		result, transcribeErr := client.Transcribe(ctx, speechmatics.Request{
			FilePath: path, Model: opts.speechmaticsModel, Language: speechmaticsLanguage(opts.language),
			Diarization: opts.diarization, SpeakerSensitivity: opts.speakerSensitivity,
			PreferCurrentSpeaker: opts.preferCurrentSpeaker, PunctuationSensitivity: opts.punctuationSensitivity,
			ExpectedLanguages: commaSeparated(opts.expectedLanguages), LanguageHints: commaSeparated(opts.languageHints),
			AdditionalVocabulary: commaSeparated(opts.vocabulary), RemoveDisfluencies: opts.removeDisfluencies,
			WaitSeconds: opts.speechmaticsWaitSeconds,
		})
		cancel()
		if transcribeErr != nil {
			if result.JobID != "" && !opts.keepRemoteJob {
				deleteSpeechmaticsJob(rootContext, client, result.JobID)
			}
			failures++
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, transcribeErr)
			var apiErr *speechmatics.APIError
			if errors.As(transcribeErr, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
				remaining := len(files) - index - 1
				failures += remaining
				if remaining > 0 {
					fmt.Fprintf(os.Stderr, "Speechmatics authentication was rejected; skipping %d remaining file(s).\n", remaining)
				}
				break
			}
			continue
		}
		prettyJSON, prettyErr := speechmatics.PrettyJSON(result.Raw)
		if prettyErr != nil {
			failures++
			fmt.Fprintf(os.Stderr, "%s: format JSON: %v\n", path, prettyErr)
			continue
		}
		paths := resultPaths(opts, path)
		var writeErr error
		written := paths.Text + " and " + paths.JSON
		if opts.diarization == "none" {
			writeErr = output.WriteResults(paths, result.Text, prettyJSON, opts.overwrite)
		} else {
			writeErr = output.WriteDiarizedResults(paths, result.Text, result.Diarized, prettyJSON, opts.overwrite)
			written = paths.Text + ", " + paths.Speakers + ", and " + paths.JSON
		}
		if writeErr != nil {
			failures++
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, writeErr)
			continue
		}
		if !opts.keepRemoteJob {
			deleteSpeechmaticsJob(rootContext, client, result.JobID)
		}
		languageInfo := ""
		if len(result.Languages) > 0 {
			languageInfo = "; language(s) " + strings.Join(result.Languages, ",")
		}
		fmt.Fprintf(os.Stderr, "    Done in %s%s; wrote %s\n", time.Since(started).Round(time.Millisecond), languageInfo, written)
		if opts.printText {
			text := result.Text
			if opts.diarization != "none" {
				text = result.Diarized
			}
			fmt.Printf("\n=== %s ===\n%s\n", filepath.Base(path), text)
		}
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "Completed with %d failed file(s).\n", failures)
		return 1
	}
	return 0
}

func deleteSpeechmaticsJob(parent context.Context, client *speechmatics.Client, jobID string) {
	cleanupContext, cleanupCancel := context.WithTimeout(parent, 30*time.Second)
	deleteErr := client.DeleteJob(cleanupContext, jobID)
	cleanupCancel()
	if deleteErr != nil {
		fmt.Fprintf(os.Stderr, "    Warning: remote Speechmatics job %s could not be deleted: %v\n", jobID, deleteErr)
	}
}

func humanBytes(size int64) string {
	if size < 1024 {
		return strconv.FormatInt(size, 10) + " B"
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(size)/(1024*1024))
}
