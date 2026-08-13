package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Settings struct {
	UserID              int64
	Language            string
	Provider            string
	DiarizationProvider string
	TranslationLanguage string
}

type Job struct {
	ID                     int64
	UserID                 int64
	ChatID                 int64
	SourceMessageID        int
	ResultMessageID        int
	FileID                 string
	FileUniqueID           string
	FileName               string
	MediaKind              string
	Provider               string
	Model                  string
	Language               string
	Diarization            bool
	Status                 string
	Text                   string
	DiarizedText           string
	DetectedLanguages      string
	Error                  string
	DurationSeconds        float64
	ProcessingMilliseconds int64
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty database path")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("protect database: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close database file: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	store := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS settings (
            user_id INTEGER PRIMARY KEY,
            language TEXT NOT NULL DEFAULT 'ru',
            provider TEXT NOT NULL DEFAULT 'groq',
            diarization_provider TEXT NOT NULL DEFAULT 'soniox',
            translation_language TEXT NOT NULL DEFAULT 'ru',
            updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
        )`,
		`CREATE TABLE IF NOT EXISTS jobs (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            user_id INTEGER NOT NULL,
            chat_id INTEGER NOT NULL,
            source_message_id INTEGER NOT NULL,
            result_message_id INTEGER NOT NULL DEFAULT 0,
            file_id TEXT NOT NULL,
            file_unique_id TEXT NOT NULL,
            file_name TEXT NOT NULL DEFAULT '',
            media_kind TEXT NOT NULL,
            provider TEXT NOT NULL,
            model TEXT NOT NULL,
            language TEXT NOT NULL,
            diarization INTEGER NOT NULL DEFAULT 0,
            status TEXT NOT NULL,
            text TEXT NOT NULL DEFAULT '',
            diarized_text TEXT NOT NULL DEFAULT '',
            detected_languages TEXT NOT NULL DEFAULT '',
            error TEXT NOT NULL DEFAULT '',
            duration_seconds REAL NOT NULL DEFAULT 0,
            processing_milliseconds INTEGER NOT NULL DEFAULT 0,
            created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
            updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
        )`,
		`CREATE INDEX IF NOT EXISTS jobs_user_created ON jobs(user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS jobs_result_message ON jobs(chat_id, result_message_id)`,
		`CREATE INDEX IF NOT EXISTS jobs_cache ON jobs(file_unique_id, provider, model, language, diarization, status)`,
		`CREATE TABLE IF NOT EXISTS translations (
            job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
            language TEXT NOT NULL,
            model TEXT NOT NULL,
            text TEXT NOT NULL,
            created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
            PRIMARY KEY(job_id, language, model)
        )`,
		`CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize database: %w", err)
		}
	}
	return nil
}

func (s *Store) Settings(ctx context.Context, userID int64) (Settings, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(user_id) VALUES (?) ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return Settings{}, fmt.Errorf("ensure settings: %w", err)
	}
	var result Settings
	err := s.db.QueryRowContext(ctx, `SELECT user_id, language, provider, diarization_provider, translation_language FROM settings WHERE user_id=?`, userID).
		Scan(&result.UserID, &result.Language, &result.Provider, &result.DiarizationProvider, &result.TranslationLanguage)
	if err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	return result, nil
}

func (s *Store) SetSetting(ctx context.Context, userID int64, field, value string) error {
	columns := map[string]string{
		"language": "language", "provider": "provider",
		"diarization_provider": "diarization_provider", "translation_language": "translation_language",
	}
	column, ok := columns[field]
	if !ok {
		return fmt.Errorf("unsupported setting %q", field)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(user_id) VALUES (?) ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return err
	}
	query := `UPDATE settings SET ` + column + `=?, updated_at=CURRENT_TIMESTAMP WHERE user_id=?`
	if _, err := s.db.ExecContext(ctx, query, value, userID); err != nil {
		return fmt.Errorf("update setting: %w", err)
	}
	return nil
}

func (s *Store) CreateJob(ctx context.Context, job Job) (int64, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO jobs(
        user_id, chat_id, source_message_id, result_message_id, file_id, file_unique_id, file_name,
        media_kind, provider, model, language, diarization, status
    ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, job.UserID, job.ChatID, job.SourceMessageID, job.ResultMessageID,
		job.FileID, job.FileUniqueID, job.FileName, job.MediaKind, job.Provider, job.Model, job.Language, job.Diarization, job.Status)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read job ID: %w", err)
	}
	return id, nil
}

func (s *Store) SetJobResultMessage(ctx context.Context, jobID int64, messageID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET result_message_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, messageID, jobID)
	return err
}

func (s *Store) MarkJobRunning(ctx context.Context, jobID int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='running', error='', updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='queued'`, jobID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *Store) CancelQueuedJobs(ctx context.Context, userID int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='canceled', error='canceled by user', updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND status='queued'`, userID)
	if err != nil {
		return 0, fmt.Errorf("cancel queued jobs: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) RequeueJob(ctx context.Context, jobID int64, reason string) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='queued', error=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='running'`, reason, jobID)
	if err != nil {
		return fmt.Errorf("requeue job: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("requeue job %d: it is no longer running", jobID)
	}
	return nil
}

// RecoverPendingJobs returns work interrupted by a process restart. Running
// jobs are made queued again because no API request can survive the process.
// Ready jobs already contain a transcript and only need Telegram publication.
func (s *Store) RecoverPendingJobs(ctx context.Context) ([]int64, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='queued', error='recovered after restart', updated_at=CURRENT_TIMESTAMP WHERE status='running'`); err != nil {
		return nil, fmt.Errorf("recover running jobs: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE status IN ('queued','ready') ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read pending jobs: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) CompleteJob(ctx context.Context, jobID int64, text, diarized, languages string, duration float64, processing time.Duration) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='ready', text=?, diarized_text=?, detected_languages=?,
        duration_seconds=?, processing_milliseconds=?, error='', updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		text, diarized, languages, duration, processing.Milliseconds(), jobID)
	return err
}

func (s *Store) MarkJobPublished(ctx context.Context, jobID int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='completed', updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='ready'`, jobID)
	if err != nil {
		return fmt.Errorf("mark job published: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("mark job %d published: it is not ready", jobID)
	}
	return nil
}

func (s *Store) FailJob(ctx context.Context, jobID int64, message string) error {
	if len(message) > 2000 {
		message = message[:2000]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='failed', error=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, message, jobID)
	return err
}

func (s *Store) Job(ctx context.Context, jobID int64) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE id=?`, jobID))
}

func (s *Store) JobByResultMessage(ctx context.Context, chatID int64, messageID int) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE chat_id=? AND result_message_id=? ORDER BY id DESC LIMIT 1`, chatID, messageID))
}

func (s *Store) CachedJob(ctx context.Context, uniqueID, provider, model, language string, diarization bool) (Job, bool, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE file_unique_id=? AND provider=? AND model=? AND language=? AND diarization=? AND status='completed' ORDER BY id DESC LIMIT 1`,
		uniqueID, provider, model, language, diarization))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	return job, err == nil, err
}

func (s *Store) Translation(ctx context.Context, jobID int64, language, model string) (string, bool, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT text FROM translations WHERE job_id=? AND language=? AND model=?`, jobID, language, model).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return text, err == nil, err
}

func (s *Store) SaveTranslation(ctx context.Context, jobID int64, language, model, text string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO translations(job_id,language,model,text) VALUES(?,?,?,?)
        ON CONFLICT(job_id,language,model) DO UPDATE SET text=excluded.text, created_at=CURRENT_TIMESTAMP`, jobID, language, model, text)
	return err
}

func (s *Store) PurgeCompletedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE status IN ('completed','failed','canceled') AND updated_at < ?`, cutoff.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return 0, fmt.Errorf("purge completed jobs: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) UpdateOffset(ctx context.Context, offset int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('telegram_update_offset',?)
        ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(offset))
	return err
}

func (s *Store) Offset(ctx context.Context) (int64, error) {
	var offset int64
	err := s.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM metadata WHERE key='telegram_update_offset'`).Scan(&offset)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return offset, err
}

const jobSelect = `SELECT id,user_id,chat_id,source_message_id,result_message_id,file_id,file_unique_id,file_name,
media_kind,provider,model,language,diarization,status,text,diarized_text,detected_languages,error,
duration_seconds,processing_milliseconds FROM jobs`

type scanner interface{ Scan(...any) error }

func scanJob(row scanner) (Job, error) {
	var job Job
	err := row.Scan(&job.ID, &job.UserID, &job.ChatID, &job.SourceMessageID, &job.ResultMessageID,
		&job.FileID, &job.FileUniqueID, &job.FileName, &job.MediaKind, &job.Provider, &job.Model,
		&job.Language, &job.Diarization, &job.Status, &job.Text, &job.DiarizedText,
		&job.DetectedLanguages, &job.Error, &job.DurationSeconds, &job.ProcessingMilliseconds)
	return job, err
}
