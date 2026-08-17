package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

	"gopkg.in/natefinch/lumberjack.v2"
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
	logFile       string
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
	flags.StringVar(&opts.logFile, "log-file", "", "optional persistent JSON log file with rotation")
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
	adminValue, err := config.Secret("TELEGRAM_ADMIN_USER_IDS")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	admins, err := parseAllowedUsers(adminValue)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: TELEGRAM_ADMIN_USER_IDS:", err)
		return 1
	}
	legacyAllowedValue, err := config.Secret("TELEGRAM_ALLOWED_USER_IDS")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	legacyAllowed, err := parseOptionalUsers(legacyAllowedValue)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: TELEGRAM_ALLOWED_USER_IDS:", err)
		return 1
	}
	level, err := parseLogLevel(opts.logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	logger, closeLogs, err := createLogger(opts.logFile, level)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer closeLogs()

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
	if err := database.InitializeAccess(context.Background(), userIDs(admins), userIDs(legacyAllowed)); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	telegramToken, err := config.Secret("TELEGRAM_BOT_TOKEN")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	telegramClient, err := telegram.NewClient(telegramToken, telegram.Options{BaseURL: opts.telegramURL})
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
		defer cancel()
		me, err := telegramClient.GetMe(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: Telegram credentials check failed:", err)
			return 1
		}
		allowed, err := database.AuthorizedUsers(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: read authorized users:", err)
			return 1
		}
		fmt.Printf("configuration OK; Telegram bot @%s (id %d); administrators: %d; allowed users: %d\n", me.Username, me.ID, len(admins), len(allowed))
		return 0
	}
	application, err := bot.New(telegramClient, database, service, bot.Config{
		AdminUsers: admins, TempDir: opts.tempDir, MaxFileBytes: opts.maxMiB << 20,
		JobTimeout: opts.jobTimeout, QueueSize: opts.queueSize, RetentionDays: opts.retentionDays,
	}, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("Tolmach bot started", "administrators", len(admins), "queue_size", opts.queueSize)
	err = application.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("Tolmach bot stopped", "error", err)
		return 1
	}
	logger.Info("Tolmach bot stopped")
	return 0
}

func createLogger(path string, level slog.Level) (*slog.Logger, func(), error) {
	var output io.Writer = os.Stderr
	closeLogs := func() {}
	if strings.TrimSpace(path) != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, closeLogs, fmt.Errorf("create log directory: %w", err)
		}
		rotating := &lumberjack.Logger{
			Filename: path, MaxSize: 20, MaxBackups: 10, MaxAge: 30, Compress: true,
		}
		output = io.MultiWriter(os.Stderr, rotating)
		closeLogs = func() { _ = rotating.Close() }
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})), closeLogs, nil
}

func createService() (*transcription.Service, error) {
	groqKey, err := config.Secret("GROQ_API_KEY")
	if err != nil {
		return nil, err
	}
	groqClient, err := groq.NewClient(groqKey, groq.Options{})
	if err != nil {
		return nil, errors.New("GROQ_API_KEY is required for default transcription and translation")
	}
	service := &transcription.Service{Groq: groqClient}
	sonioxKey, err := config.Secret("SONIOX_API_KEY")
	if err != nil {
		return nil, err
	}
	if key := sonioxKey; key != "" {
		client, err := soniox.NewClient(key, soniox.Options{})
		if err != nil {
			return nil, fmt.Errorf("configure Soniox: %w", err)
		}
		service.Soniox = client
	}
	speechmaticsKey, err := config.Secret("SPEECHMATICS_API_KEY")
	if err != nil {
		return nil, err
	}
	if key := speechmaticsKey; key != "" {
		client, err := speechmatics.NewClient(key, speechmatics.Options{})
		if err != nil {
			return nil, fmt.Errorf("configure Speechmatics: %w", err)
		}
		service.Speechmatics = client
	}
	return service, nil
}

func parseAllowedUsers(value string) (map[int64]bool, error) {
	result, err := parseOptionalUsers(value)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.New("at least one user ID is required")
	}
	return result, nil
}

func parseOptionalUsers(value string) (map[int64]bool, error) {
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
	return result, nil
}

func userIDs(users map[int64]bool) []int64 {
	result := make([]int64, 0, len(users))
	for userID := range users {
		result = append(result, userID)
	}
	return result
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
