package output

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteResults(t *testing.T) {
	dir := t.TempDir()
	paths := ResultPaths(dir, "/input/voice.ogg", "whisper-large-v3-turbo", "ru")
	if err := WriteResults(paths, " Привет! ", []byte("{}\n"), false); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(paths.Text) != "voice.whisper-large-v3-turbo.ru.transcript.txt" {
		t.Fatalf("text path = %s", paths.Text)
	}
	contents, err := os.ReadFile(paths.Text)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "Привет!\n" {
		t.Fatalf("contents = %q", contents)
	}
	if err := WriteResults(paths, "again", []byte("{}"), false); err == nil {
		t.Fatal("expected existing output error")
	}
}

func TestWriteDiarizedResults(t *testing.T) {
	dir := t.TempDir()
	paths := ResultPaths(dir, "call.ogg", "speechmatics-enhanced-speaker", "ru")
	if err := WriteDiarizedResults(paths, "Привет.", "[00:00–00:01] S1:\nПривет.", []byte("{}\n"), false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths.Speakers)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[00:00–00:01] S1:\nПривет.\n" {
		t.Fatalf("speakers = %q", data)
	}
}

func TestPolishedPathAndWriteText(t *testing.T) {
	dir := t.TempDir()
	paths := ResultPaths(dir, "voice.ogg", "model", "ru")
	path := PolishedPath(paths)
	if filepath.Base(path) != "voice.model.ru.transcript.polished.txt" {
		t.Fatalf("path = %s", path)
	}
	if err := WriteText(path, " Исправлено. ", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "Исправлено.\n" {
		t.Fatalf("data = %q, error = %v", data, err)
	}
}
