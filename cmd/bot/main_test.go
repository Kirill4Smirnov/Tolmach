package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAllowedUsersAndLogLevel(t *testing.T) {
	users, err := parseAllowedUsers("123, 456,123")
	if err != nil || len(users) != 2 || !users[123] || !users[456] {
		t.Fatalf("users=%#v err=%v", users, err)
	}
	if _, err := parseAllowedUsers(""); err == nil {
		t.Fatal("expected empty allowlist error")
	}
	if level, err := parseLogLevel("warn"); err != nil || level != slog.LevelWarn {
		t.Fatalf("level=%v err=%v", level, err)
	}
}

func TestCreateLoggerWritesPersistentJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "tolmach.jsonl")
	logger, closeLogs, err := createLogger(path, slog.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("test event", "job_id", 7)
	closeLogs()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"msg":"test event"`) || !strings.Contains(string(data), `"job_id":7`) {
		t.Fatalf("log = %s", data)
	}
}
