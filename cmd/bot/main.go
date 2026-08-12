package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tolmach/internal/bot"
	"tolmach/internal/config"
	"tolmach/internal/groq"
	"tolmach/internal/soniox"
	"tolmach/internal/speechmatics"
	"tolmach/internal/store"
	"tolmach/internal/telegram"
	"tolmach/internal/transcription"
)

type options struct {
	dotEnv        string
	databasePath  string
	tempDir       string
	maxMiB        int64
	queueSize     int
	jobTimeout    time.Duration
	telegramURL   string
	logLevel      string
	retentionDays int
	check         bool
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	var opts options
	flags := flag.NewFlagSet("tolmach-bot", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&opts.dotEnv, "env-file", ".env", "dotenv file")
	flags.StringVar(&opts.databasePath, "database", "data/tolmach.db", "SQLite database path")
	flags.StringVar(&opts.tempDir, "temp-dir", "", "private temporary file parent; system temp by default")
	flags.Int64Var(&opts.maxMiB, "max-mib", 20, "maximum Telegram media size in MiB")
	flags.IntVar(&opts.queueSize, "queue-size", 32, "maximum queued jobs")
	flags.DurationVar(&opts.jobTimeout, "job-timeout", 15*time.Minute, "download and transcription timeout")
	flags.StringVar(&opts.telegramURL, "telegram-base-url", "", "override Telegram Bot API URL")
	flags.StringVar(&opts.logLevel, "log-level", "info", "debug, info, warn, or error")
	flags.IntVar(&opts.retentionDays, "retention-days", 7, "days to retain completed transcripts and translations")
	flags.BoolVar(&opts.check, "check", false, "validate configuration and Telegram credentials, then exit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "error: unexpected positional arguments")
		return 2
	}
	if opts.maxMiB <= 0 || opts.queueSize <= 0 || opts.jobTimeout <= 0 || opts.retentionDays <= 0 {
		fmt.Fprintln(os.Stderr, "error: limits and timeout must be positive")
		return 2
	}
	if err := config.LoadDotEnv(opts.dotEnv); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	allowed, err := parseAllowedUsers(os.Getenv("TELEGRAM_ALLOWED_USER_IDS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: TELEGRAM_ALLOWED_USER_IDS:", err)
		return 1
	}
	level, err := parseLogLevel(opts.logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if directory := filepath.Dir(opts.databasePath); directory != "." {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "error: create data directory:", err)
			return 1
		}
	}
	database, err := store.Open(opts.databasePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer database.Close()

	telegramClient, err := telegram.NewClient(os.Getenv("TELEGRAM_BOT_TOKEN"), telegram.Options{BaseURL: opts.telegramURL})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: TELEGRAM_BOT_TOKEN is missing or invalid")
		return 1
	}
	service, err := createService()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if opts.check {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		me, err := telegramClient.GetMe(ctx)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: Telegram credentials check failed:", err)
			return 1
		}
		fmt.Printf("configuration OK; Telegram bot @%s (id %d); allowed users: %d\n", me.Username, me.ID, len(allowed))
		return 0
	}
	application, err := bot.New(telegramClient, database, service, bot.Config{
		AllowedUsers: allowed, TempDir: opts.tempDir, MaxFileBytes: opts.maxMiB << 20,
		JobTimeout: opts.jobTimeout, QueueSize: opts.queueSize, RetentionDays: opts.retentionDays,
	}, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("Tolmach bot started", "allowed_users", len(allowed), "queue_size", opts.queueSize)
	err = application.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("Tolmach bot stopped", "error", err)
		return 1
	}
	logger.Info("Tolmach bot stopped")
	return 0
}

func createService() (*transcription.Service, error) {
	groqClient, err := groq.NewClient(os.Getenv("GROQ_API_KEY"), groq.Options{})
	if err != nil {
		return nil, errors.New("GROQ_API_KEY is required for default transcription and translation")
	}
	service := &transcription.Service{Groq: groqClient}
	if key := strings.TrimSpace(os.Getenv("SONIOX_API_KEY")); key != "" {
		client, err := soniox.NewClient(key, soniox.Options{})
		if err != nil {
			return nil, fmt.Errorf("configure Soniox: %w", err)
		}
		service.Soniox = client
	}
	if key := strings.TrimSpace(os.Getenv("SPEECHMATICS_API_KEY")); key != "" {
		client, err := speechmatics.NewClient(key, speechmatics.Options{})
		if err != nil {
			return nil, fmt.Errorf("configure Speechmatics: %w", err)
		}
		service.Speechmatics = client
	}
	return service, nil
}

func parseAllowedUsers(value string) (map[int64]bool, error) {
	result := make(map[int64]bool)
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		id, err := strconv.ParseInt(item, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid Telegram user ID %q", item)
		}
		result[id] = true
	}
	if len(result) == 0 {
		return nil, errors.New("at least one user ID is required")
	}
	return result, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", value)
	}
}
