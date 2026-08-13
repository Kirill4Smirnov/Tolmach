package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// LoadDotEnv reads a small, conventional KEY=VALUE file. Existing environment
// variables always win so production configuration is never overwritten.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !validEnvKey(key) {
			return fmt.Errorf("%s:%d: invalid environment variable name", path, lineNumber)
		}
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("set %s: %w", key, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// Secret reads NAME_FILE first (Docker/Kubernetes secret convention), then
// falls back to NAME for local development. Secret files must be small and
// contain a single value; trailing whitespace is removed.
func Secret(name string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open %s_FILE: %w", name, err)
		}
		defer file.Close()
		value, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", name, err)
		}
		if len(value) > 64<<10 {
			return "", fmt.Errorf("%s_FILE exceeds 64 KiB", name)
		}
		return strings.TrimSpace(string(value)), nil
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}
