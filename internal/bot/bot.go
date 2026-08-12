package bot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tolmach/internal/store"
	"tolmach/internal/telegram"
	"tolmach/internal/transcription"
)

const telegramTextLimit = 4000

type Config struct {
	AllowedUsers  map[int64]bool
	TempDir       string
	MaxFileBytes  int64
	JobTimeout    time.Duration
	QueueSize     int
	RetentionDays int
}

type Engine interface {
	Transcribe(context.Context, transcription.Request) (transcription.Result, error)
	Translate(context.Context, string, string) (string, error)
}

type Bot struct {
	telegram *telegram.Client
	store    *store.Store
	service  Engine
	config   Config
	logger   *slog.Logger
	queue    chan int64
	cancelMu sync.Mutex
	cancels  map[int64]context.CancelFunc
}

func New(client *telegram.Client, database *store.Store, service Engine, config Config, logger *slog.Logger) (*Bot, error) {
	if client == nil || database == nil || service == nil {
		return nil, errors.New("bot dependencies cannot be nil")
	}
	if len(config.AllowedUsers) == 0 {
		return nil, errors.New("Telegram allowlist cannot be empty")
	}
	if config.MaxFileBytes <= 0 {
		config.MaxFileBytes = 25 << 20
	}
	if config.JobTimeout <= 0 {
		config.JobTimeout = 15 * time.Minute
	}
	if config.QueueSize <= 0 {
		config.QueueSize = 32
	}
	if config.RetentionDays <= 0 {
		config.RetentionDays = 7
	}
	if config.TempDir == "" {
		config.TempDir = os.TempDir()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Bot{telegram: client, store: database, service: service, config: config, logger: logger,
		queue: make(chan int64, config.QueueSize), cancels: make(map[int64]context.CancelFunc)}, nil
}

func (b *Bot) Run(ctx context.Context) error {
	if _, err := b.store.PurgeCompletedBefore(ctx, time.Now().UTC().AddDate(0, 0, -b.config.RetentionDays)); err != nil {
		return fmt.Errorf("purge expired transcripts: %w", err)
	}
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		b.maintenance(ctx)
	}()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		b.worker(ctx)
	}()
	pending, err := b.store.RecoverPendingJobs(ctx)
	if err != nil {
		return fmt.Errorf("recover pending jobs: %w", err)
	}
	for _, jobID := range pending {
		select {
		case b.queue <- jobID:
		case <-ctx.Done():
			b.cancelAll()
			<-workerDone
			return ctx.Err()
		}
	}
	if len(pending) > 0 {
		b.logger.Info("Pending jobs recovered", "jobs", len(pending))
	}
	offset, err := b.store.Offset(ctx)
	if err != nil {
		return fmt.Errorf("read Telegram offset: %w", err)
	}
	for ctx.Err() == nil {
		pollCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		updates, pollErr := b.telegram.GetUpdates(pollCtx, offset, 50)
		cancel()
		if pollErr != nil {
			if ctx.Err() != nil {
				break
			}
			b.logger.Error("Telegram polling failed", "error", pollErr)
			if !wait(ctx, 2*time.Second) {
				break
			}
			continue
		}
		for _, update := range updates {
			if err := b.HandleUpdate(ctx, update); err != nil {
				b.logger.Error("Telegram update failed", "update_id", update.UpdateID, "error", err)
			}
			offset = update.UpdateID + 1
			if err := b.store.UpdateOffset(ctx, offset); err != nil {
				return fmt.Errorf("save Telegram offset: %w", err)
			}
		}
	}
	b.cancelAll()
	<-workerDone
	<-maintenanceDone
	return ctx.Err()
}

func (b *Bot) maintenance(ctx context.Context) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			deleted, err := b.store.PurgeCompletedBefore(ctx, now.UTC().AddDate(0, 0, -b.config.RetentionDays))
			if err != nil {
				b.logger.Error("Transcript retention cleanup failed", "error", err)
			} else if deleted > 0 {
				b.logger.Info("Expired transcripts deleted", "jobs", deleted)
			}
		}
	}
}

func (b *Bot) HandleUpdate(ctx context.Context, update telegram.Update) error {
	if update.CallbackQuery != nil {
		return b.handleCallback(ctx, update.CallbackQuery)
	}
	if update.Message == nil || update.Message.From == nil {
		return nil
	}
	message := update.Message
	if !b.config.AllowedUsers[message.From.ID] {
		b.logger.Warn("Rejected user", "user_id", message.From.ID)
		return nil
	}
	if message.Chat.Type != "private" {
		_, err := b.telegram.SendMessage(ctx, telegram.SendMessageRequest{ChatID: message.Chat.ID, Text: "Пока я работаю только в личных чатах."})
		return err
	}
	if strings.HasPrefix(strings.TrimSpace(message.Text), "/") {
		return b.handleCommand(ctx, message)
	}
	media, kind, ok := mediaFromMessage(message)
	if !ok {
		return nil
	}
	if media.FileSize > b.config.MaxFileBytes && media.FileSize > 0 {
		_, err := b.telegram.SendMessage(ctx, telegram.SendMessageRequest{ChatID: message.Chat.ID,
			ReplyToMessageID: message.MessageID, Text: fmt.Sprintf("Файл слишком большой: лимит %.0f МБ.", float64(b.config.MaxFileBytes)/(1<<20))})
		return err
	}
	settings, err := b.store.Settings(ctx, message.From.ID)
	if err != nil {
		return err
	}
	return b.createAndQueue(ctx, store.Job{
		UserID: message.From.ID, ChatID: message.Chat.ID, SourceMessageID: message.MessageID,
		FileID: media.FileID, FileUniqueID: media.FileUniqueID, FileName: media.FileName, MediaKind: kind,
		Provider: settings.Provider, Model: transcription.Model(settings.Provider), Language: settings.Language,
		Diarization: false, Status: "queued",
	})
}

func (b *Bot) handleCommand(ctx context.Context, message *telegram.Message) error {
	command, argument := parseCommand(message.Text)
	switch command {
	case "start", "help":
		return b.sendText(ctx, message.Chat.ID, message.MessageID, helpText())
	case "settings":
		settings, err := b.store.Settings(ctx, message.From.ID)
		if err != nil {
			return err
		}
		return b.sendText(ctx, message.Chat.ID, message.MessageID, settingsText(settings))
	case "language":
		if argument == "" {
			return b.sendText(ctx, message.Chat.ID, message.MessageID, "Использование: /language ru или /language auto")
		}
		language := strings.ToLower(argument)
		if !validLanguage(language) {
			return b.sendText(ctx, message.Chat.ID, message.MessageID, "Нужен код языка из 2–8 латинских символов либо auto.")
		}
		if err := b.store.SetSetting(ctx, message.From.ID, "language", language); err != nil {
			return err
		}
		return b.sendText(ctx, message.Chat.ID, message.MessageID, "Язык распознавания: "+language)
	case "provider":
		if !validProvider(argument, true) {
			return b.sendText(ctx, message.Chat.ID, message.MessageID, "Использование: /provider groq, /provider soniox или /provider speechmatics")
		}
		if err := b.store.SetSetting(ctx, message.From.ID, "provider", argument); err != nil {
			return err
		}
		return b.sendText(ctx, message.Chat.ID, message.MessageID, "Основной провайдер: "+argument)
	case "diarization_provider":
		if !validProvider(argument, false) {
			return b.sendText(ctx, message.Chat.ID, message.MessageID, "Использование: /diarization_provider soniox или /diarization_provider speechmatics")
		}
		if err := b.store.SetSetting(ctx, message.From.ID, "diarization_provider", argument); err != nil {
			return err
		}
		return b.sendText(ctx, message.Chat.ID, message.MessageID, "Провайдер диаризации: "+argument)
	case "translate":
		return b.handleTranslate(ctx, message, strings.ToLower(argument))
	case "cancel":
		return b.handleCancel(ctx, message)
	default:
		return b.sendText(ctx, message.Chat.ID, message.MessageID, "Неизвестная команда. Используйте /help.")
	}
}

func (b *Bot) handleTranslate(ctx context.Context, message *telegram.Message, target string) error {
	if message.ReplyToMessage == nil {
		return b.sendText(ctx, message.Chat.ID, message.MessageID, "Ответьте командой /translate en на сообщение с транскрипцией.")
	}
	settings, err := b.store.Settings(ctx, message.From.ID)
	if err != nil {
		return err
	}
	if target == "" {
		target = settings.TranslationLanguage
	}
	if !validLanguage(target) || target == "auto" {
		return b.sendText(ctx, message.Chat.ID, message.MessageID, "Для перевода укажите язык, например /translate en.")
	}
	job, err := b.store.JobByResultMessage(ctx, message.Chat.ID, message.ReplyToMessage.MessageID)
	if errors.Is(err, sql.ErrNoRows) || job.UserID != message.From.ID {
		return b.sendText(ctx, message.Chat.ID, message.MessageID, "Не нашёл связанную транскрипцию. Ответьте на первое сообщение результата.")
	}
	if err != nil {
		return err
	}
	text := job.Text
	if job.Diarization && job.DiarizedText != "" {
		text = job.DiarizedText
	}
	translated, found, err := b.store.Translation(ctx, job.ID, target, transcription.TranslationModel)
	if err != nil {
		return err
	}
	if !found {
		status, err := b.telegram.SendMessage(ctx, telegram.SendMessageRequest{ChatID: message.Chat.ID, ReplyToMessageID: message.MessageID, Text: "Перевожу на " + target + "…"})
		if err != nil {
			return err
		}
		translateCtx, cancel := context.WithTimeout(ctx, b.config.JobTimeout)
		translated, err = b.service.Translate(translateCtx, text, target)
		cancel()
		if err != nil {
			_ = b.telegram.EditMessage(ctx, message.Chat.ID, status.MessageID, "Не удалось перевести запись.", nil)
			return err
		}
		if err := b.store.SaveTranslation(ctx, job.ID, target, transcription.TranslationModel, translated); err != nil {
			return err
		}
		if err := b.store.SetSetting(ctx, message.From.ID, "translation_language", target); err != nil {
			return err
		}
		chunks := telegram.SplitText("Перевод · "+target+"\n\n"+translated, telegramTextLimit)
		if err := b.telegram.EditMessage(ctx, message.Chat.ID, status.MessageID, chunks[0], nil); err != nil {
			return err
		}
		return b.sendChunks(ctx, message.Chat.ID, 0, chunks[1:])
	}
	return b.sendChunks(ctx, message.Chat.ID, message.MessageID, telegram.SplitText("Перевод · "+target+"\n\n"+translated, telegramTextLimit))
}

func (b *Bot) handleCancel(ctx context.Context, message *telegram.Message) error {
	queued, err := b.store.CancelQueuedJobs(ctx, message.From.ID)
	if err != nil {
		return err
	}
	b.cancelMu.Lock()
	var canceled bool
	for jobID, cancel := range b.cancels {
		job, err := b.store.Job(ctx, jobID)
		if err == nil && job.UserID == message.From.ID {
			cancel()
			canceled = true
		}
	}
	b.cancelMu.Unlock()
	if !canceled && queued == 0 {
		return b.sendText(ctx, message.Chat.ID, message.MessageID, "Сейчас у вас нет активной обработки.")
	}
	return b.sendText(ctx, message.Chat.ID, message.MessageID, "Отменяю обработку и ожидающие задания.")
}

func (b *Bot) handleCallback(ctx context.Context, callback *telegram.CallbackQuery) error {
	if !b.config.AllowedUsers[callback.From.ID] || callback.Message == nil || callback.Message.Chat.Type != "private" {
		return b.telegram.AnswerCallback(ctx, callback.ID, "Недоступно", true)
	}
	parts := strings.Split(callback.Data, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return b.telegram.AnswerCallback(ctx, callback.ID, "Кнопка устарела", true)
	}
	jobID, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil {
		return b.telegram.AnswerCallback(ctx, callback.ID, "Кнопка устарела", true)
	}
	original, err := b.store.Job(ctx, jobID)
	if err != nil || original.UserID != callback.From.ID {
		return b.telegram.AnswerCallback(ctx, callback.ID, "Транскрипция не найдена", true)
	}
	switch parts[0] {
	case "d":
		settings, settingsErr := b.store.Settings(ctx, callback.From.ID)
		if settingsErr != nil {
			return settingsErr
		}
		if err := b.telegram.AnswerCallback(ctx, callback.ID, "Запускаю диаризацию", false); err != nil {
			return err
		}
		return b.createAndQueue(ctx, cloneJob(original, settings.DiarizationProvider, true))
	case "r":
		markup := providerKeyboard(jobID)
		if err := b.telegram.EditMessage(ctx, callback.Message.Chat.ID, callback.Message.MessageID, renderJob(original), markup); err != nil {
			return b.telegram.AnswerCallback(ctx, callback.ID, "Не удалось открыть список", true)
		}
		return b.telegram.AnswerCallback(ctx, callback.ID, "Выберите провайдера ниже", false)
	case "p":
		if !validProvider(parts[1], true) {
			return b.telegram.AnswerCallback(ctx, callback.ID, "Неизвестный провайдер", true)
		}
		if err := b.telegram.AnswerCallback(ctx, callback.ID, "Перераспознаю через "+parts[1], false); err != nil {
			return err
		}
		return b.createAndQueue(ctx, cloneJob(original, parts[1], parts[1] != "groq" && original.Diarization))
	default:
		return b.telegram.AnswerCallback(ctx, callback.ID, "Кнопка устарела", true)
	}
}

func cloneJob(original store.Job, provider string, diarization bool) store.Job {
	return store.Job{UserID: original.UserID, ChatID: original.ChatID, SourceMessageID: original.SourceMessageID,
		FileID: original.FileID, FileUniqueID: original.FileUniqueID, FileName: original.FileName, MediaKind: original.MediaKind,
		Provider: provider, Model: transcription.Model(provider), Language: original.Language, Diarization: diarization, Status: "queued"}
}

func (b *Bot) createAndQueue(ctx context.Context, job store.Job) error {
	status, err := b.telegram.SendMessage(ctx, telegram.SendMessageRequest{ChatID: job.ChatID, ReplyToMessageID: job.SourceMessageID,
		Text: telegram.FormatQueuePosition(len(b.queue))})
	if err != nil {
		return err
	}
	job.ResultMessageID = status.MessageID
	jobID, err := b.store.CreateJob(ctx, job)
	if err != nil {
		return err
	}
	select {
	case b.queue <- jobID:
		return nil
	default:
		_ = b.store.FailJob(ctx, jobID, "queue is full")
		_ = b.telegram.EditMessage(ctx, job.ChatID, status.MessageID, "Очередь заполнена. Попробуйте немного позже.", nil)
		return errors.New("transcription queue is full")
	}
}

func (b *Bot) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case jobID := <-b.queue:
			b.processJob(ctx, jobID)
		}
	}
}

func (b *Bot) processJob(parent context.Context, jobID int64) {
	job, err := b.store.Job(parent, jobID)
	if err != nil {
		b.logger.Error("Read queued job failed", "job_id", jobID, "error", err)
		return
	}
	running, err := b.store.MarkJobRunning(parent, job.ID)
	if err != nil {
		b.failJob(parent, job, err)
		return
	}
	if cached, found, cacheErr := b.store.CachedJob(parent, job.FileUniqueID, job.Provider, job.Model, job.Language, job.Diarization); cacheErr == nil && found && cached.ID != job.ID {
		_ = b.store.CompleteJob(parent, job.ID, cached.Text, cached.DiarizedText, cached.DetectedLanguages, cached.DurationSeconds, 0)
		job.Text, job.DiarizedText, job.DetectedLanguages, job.DurationSeconds, job.Status = cached.Text, cached.DiarizedText, cached.DetectedLanguages, cached.DurationSeconds, "completed"
		b.publishResult(parent, job, true)
		return
	}
	if !running {
		_ = b.telegram.EditMessage(parent, job.ChatID, job.ResultMessageID, "Обработка отменена.", nil)
		return
	}
	_ = b.telegram.EditMessage(parent, job.ChatID, job.ResultMessageID, "Распознаю через "+providerName(job.Provider)+"…", nil)
	jobCtx, cancel := context.WithTimeout(parent, b.config.JobTimeout)
	b.cancelMu.Lock()
	b.cancels[job.ID] = cancel
	b.cancelMu.Unlock()
	defer func() {
		cancel()
		b.cancelMu.Lock()
		delete(b.cancels, job.ID)
		b.cancelMu.Unlock()
	}()
	remote, err := b.telegram.GetFile(jobCtx, job.FileID)
	if err != nil {
		b.failJob(parent, job, err)
		return
	}
	tempDir, err := os.MkdirTemp(b.config.TempDir, "tolmach-job-*")
	if err != nil {
		b.failJob(parent, job, err)
		return
	}
	defer os.RemoveAll(tempDir)
	if err := os.Chmod(tempDir, 0o700); err != nil {
		b.failJob(parent, job, err)
		return
	}
	tempPath := filepath.Join(tempDir, "media"+mediaExtension(job, remote.FilePath))
	if err := b.telegram.Download(jobCtx, remote.FilePath, tempPath, b.config.MaxFileBytes); err != nil {
		b.failJob(parent, job, err)
		return
	}
	started := time.Now()
	result, err := b.service.Transcribe(jobCtx, transcription.Request{FilePath: tempPath, Provider: job.Provider, Language: job.Language, Diarization: job.Diarization})
	if err != nil {
		b.failJob(parent, job, err)
		return
	}
	if result.Warning != nil {
		b.logger.Warn("Transcription completed with cleanup warning", "job_id", job.ID, "provider", job.Provider, "error", result.Warning)
	}
	processing := time.Since(started)
	languages := strings.Join(result.Languages, ",")
	if err := b.store.CompleteJob(parent, job.ID, result.Text, result.Diarized, languages, result.Duration, processing); err != nil {
		b.failJob(parent, job, err)
		return
	}
	job.Text, job.DiarizedText, job.DetectedLanguages, job.DurationSeconds = result.Text, result.Diarized, languages, result.Duration
	job.Status = "completed"
	b.publishResult(parent, job, false)
}

func (b *Bot) failJob(ctx context.Context, job store.Job, err error) {
	_ = b.store.FailJob(ctx, job.ID, err.Error())
	message := "Не удалось распознать запись. Попробуйте другой провайдер или повторите позже."
	if errors.Is(err, context.Canceled) {
		message = "Обработка отменена."
	} else if errors.Is(err, context.DeadlineExceeded) {
		message = "Превышено время обработки. Попробуйте ещё раз."
	}
	_ = b.telegram.EditMessage(ctx, job.ChatID, job.ResultMessageID, message, nil)
	b.logger.Error("Transcription failed", "job_id", job.ID, "provider", job.Provider, "error", err)
}

func (b *Bot) publishResult(ctx context.Context, job store.Job, cached bool) {
	text := job.Text
	if job.Diarization && job.DiarizedText != "" {
		text = job.DiarizedText
	}
	header := "Транскрипция · " + providerName(job.Provider)
	if job.Language != "" {
		header += " · " + job.Language
	}
	if cached {
		header += " · из кеша"
	}
	chunks := telegram.SplitText(header+"\n\n"+text, telegramTextLimit)
	markup := resultKeyboard(job.ID, !job.Diarization)
	if err := b.telegram.EditMessage(ctx, job.ChatID, job.ResultMessageID, chunks[0], markup); err != nil {
		b.logger.Error("Publish result failed", "job_id", job.ID, "error", err)
		return
	}
	if err := b.sendChunks(ctx, job.ChatID, 0, chunks[1:]); err != nil {
		b.logger.Error("Publish result continuation failed", "job_id", job.ID, "error", err)
	}
	if len(chunks) > 1 {
		if err := b.sendTranscriptDocument(ctx, job, text); err != nil {
			b.logger.Error("Publish transcript document failed", "job_id", job.ID, "error", err)
		}
	}
}

func (b *Bot) sendTranscriptDocument(ctx context.Context, job store.Job, text string) error {
	directory, err := os.MkdirTemp(b.config.TempDir, "tolmach-output-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	name := "transcript-" + strconv.FormatInt(job.ID, 10) + ".txt"
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(strings.TrimSpace(text)+"\n"), 0o600); err != nil {
		return err
	}
	_, err = b.telegram.SendDocument(ctx, job.ChatID, path, name, "Полная транскрипция")
	return err
}

func (b *Bot) sendText(ctx context.Context, chatID int64, replyID int, text string) error {
	return b.sendChunks(ctx, chatID, replyID, telegram.SplitText(text, telegramTextLimit))
}

func (b *Bot) sendChunks(ctx context.Context, chatID int64, replyID int, chunks []string) error {
	for index, chunk := range chunks {
		request := telegram.SendMessageRequest{ChatID: chatID, Text: chunk}
		if index == 0 {
			request.ReplyToMessageID = replyID
		}
		if _, err := b.telegram.SendMessage(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

func mediaFromMessage(message *telegram.Message) (telegram.File, string, bool) {
	switch {
	case message.Voice != nil:
		return *message.Voice, "voice", true
	case message.VideoNote != nil:
		return *message.VideoNote, "video_note", true
	case message.Audio != nil:
		return *message.Audio, "audio", true
	case message.Video != nil:
		return *message.Video, "video", true
	case message.Document != nil && supportedDocument(*message.Document):
		return *message.Document, "document", true
	default:
		return telegram.File{}, "", false
	}
}

func supportedDocument(file telegram.File) bool {
	mediaType, _, _ := mime.ParseMediaType(file.MimeType)
	return strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/")
}

func mediaExtension(job store.Job, remotePath string) string {
	for _, name := range []string{job.FileName, remotePath} {
		if extension := filepath.Ext(name); len(extension) > 1 && len(extension) <= 10 {
			extension = strings.ToLower(extension)
			// Telegram commonly names Opus voice messages .oga. Groq validates
			// uploads by extension and accepts the same OGG container as .ogg.
			if extension == ".oga" {
				return ".ogg"
			}
			return extension
		}
	}
	if job.MediaKind == "voice" {
		return ".ogg"
	}
	return ".mp4"
}

func resultKeyboard(jobID int64, allowDiarization bool) *telegram.InlineKeyboardMarkup {
	row := []telegram.InlineKeyboardButton{}
	if allowDiarization {
		row = append(row, telegram.InlineKeyboardButton{Text: "👥 Разделить по спикерам", CallbackData: "d:" + strconv.FormatInt(jobID, 10)})
	}
	row = append(row, telegram.InlineKeyboardButton{Text: "🔄 Перераспознать", CallbackData: "r:" + strconv.FormatInt(jobID, 10)})
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{row}}
}

func providerKeyboard(jobID int64) *telegram.InlineKeyboardMarkup {
	id := strconv.FormatInt(jobID, 10)
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "Groq Large v3", CallbackData: "p:groq:" + id}},
		{{Text: "Soniox", CallbackData: "p:soniox:" + id}, {Text: "Speechmatics", CallbackData: "p:speechmatics:" + id}},
	}}
}

func renderJob(job store.Job) string {
	text := job.Text
	if job.Diarization && job.DiarizedText != "" {
		text = job.DiarizedText
	}
	return telegram.SplitText("Транскрипция · "+providerName(job.Provider)+" · "+job.Language+"\n\n"+text, telegramTextLimit)[0]
}

func providerName(provider string) string {
	switch provider {
	case "groq":
		return "Groq Large v3"
	case "soniox":
		return "Soniox V5"
	case "speechmatics":
		return "Speechmatics Enhanced"
	default:
		return provider
	}
}

func parseCommand(text string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return "", ""
	}
	command := strings.TrimPrefix(strings.ToLower(fields[0]), "/")
	command, _, _ = strings.Cut(command, "@")
	argument := ""
	if len(fields) > 1 {
		argument = strings.TrimSpace(fields[1])
	}
	return command, argument
}

func validLanguage(language string) bool {
	if language == "auto" {
		return true
	}
	if len(language) < 2 || len(language) > 8 {
		return false
	}
	for _, r := range language {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func validProvider(provider string, includeGroq bool) bool {
	return provider == "soniox" || provider == "speechmatics" || includeGroq && provider == "groq"
}

func settingsText(settings store.Settings) string {
	return fmt.Sprintf("Настройки\n\nПровайдер: %s\nДиаризация: %s\nЯзык: %s\nЯзык перевода: %s\n\nИзменение:\n/provider groq\n/diarization_provider soniox\n/language ru или /language auto",
		settings.Provider, settings.DiarizationProvider, settings.Language, settings.TranslationLanguage)
}

func helpText() string {
	return "Отправьте голосовое сообщение, видеокружок, аудио или видео — я верну транскрипцию.\n\n" +
		"После результата можно включить разделение по спикерам или перераспознать запись.\n\n" +
		"Команды:\n/settings — текущие настройки\n/language ru — язык записи\n/language auto — автоопределение\n/provider groq — основной провайдер\n/diarization_provider soniox — провайдер спикеров\n/translate en — перевод; отправьте ответом на транскрипцию\n/cancel — отменить активную обработку"
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (b *Bot) cancelAll() {
	b.cancelMu.Lock()
	defer b.cancelMu.Unlock()
	for _, cancel := range b.cancels {
		cancel()
	}
}
