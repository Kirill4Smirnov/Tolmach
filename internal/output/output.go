package output

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

type Paths struct {
	Text     string
	JSON     string
	Speakers string
}

func ResultPaths(outputDir, inputPath, model, language string) Paths {
	base := filepath.Base(inputPath)
	extension := filepath.Ext(base)
	stem := strings.TrimSuffix(base, extension)
	suffix := sanitize(model) + "." + sanitize(language)
	if language == "" {
		suffix = sanitize(model) + ".auto"
	}
	prefix := filepath.Join(outputDir, stem+"."+suffix+".transcript")
	return Paths{Text: prefix + ".txt", JSON: prefix + ".json", Speakers: prefix + ".speakers.txt"}
}

func WriteResults(paths Paths, text string, jsonData []byte, overwrite bool) error {
	return writeResults(paths, text, jsonData, "", false, overwrite)
}

func WriteDiarizedResults(paths Paths, text, diarized string, jsonData []byte, overwrite bool) error {
	return writeResults(paths, text, jsonData, diarized, true, overwrite)
}

func writeResults(paths Paths, text string, jsonData []byte, diarized string, includeSpeakers, overwrite bool) error {
	outputs := []string{paths.Text, paths.JSON}
	if includeSpeakers {
		outputs = append(outputs, paths.Speakers)
	}
	if !overwrite {
		for _, path := range outputs {
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("output already exists: %s (use --overwrite)", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("check output %s: %w", path, err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(paths.Text), 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := atomicWrite(paths.Text, []byte(strings.TrimSpace(text)+"\n")); err != nil {
		return err
	}
	if err := atomicWrite(paths.JSON, jsonData); err != nil {
		_ = os.Remove(paths.Text)
		return err
	}
	if includeSpeakers {
		if err := atomicWrite(paths.Speakers, []byte(strings.TrimSpace(diarized)+"\n")); err != nil {
			_ = os.Remove(paths.Text)
			_ = os.Remove(paths.JSON)
			return err
		}
	}
	return nil
}

func PolishedPath(paths Paths) string {
	return strings.TrimSuffix(paths.Text, ".txt") + ".polished.txt"
}

func WriteText(path, text string, overwrite bool) error {
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("output already exists: %s (use --overwrite)", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check output %s: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	return atomicWrite(path, []byte(strings.TrimSpace(text)+"\n"))
}

func atomicWrite(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".tolmach-result-*")
	if err != nil {
		return fmt.Errorf("create temporary output: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("commit %s: %w", path, err)
	}
	return nil
}

func sanitize(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var result strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			result.WriteRune(r)
		} else {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}
