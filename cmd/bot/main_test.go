package main

import (
	"log/slog"
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
