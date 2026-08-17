package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsJobsCacheAndTranslations(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	settings, err := store.Settings(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Provider != "groq" || settings.Language != "ru" || settings.DiarizationProvider != "soniox" {
		t.Fatalf("defaults = %#v", settings)
	}
	if err := store.SetSetting(ctx, 42, "language", "auto"); err != nil {
		t.Fatal(err)
	}
	settings, _ = store.Settings(ctx, 42)
	if settings.Language != "auto" {
		t.Fatalf("language = %q", settings.Language)
	}
	job := Job{UserID: 42, ChatID: 7, SourceMessageID: 10, ResultMessageID: 11, FileID: "file", FileUniqueID: "unique",
		MediaKind: "voice", Provider: "groq", Model: "whisper-large-v3", Language: "auto", Status: "queued"}
	id, err := store.CreateJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteJob(ctx, id, "Текст", "", "ru", 5.5, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJobPublished(ctx, id); err != nil {
		t.Fatal(err)
	}
	cached, found, err := store.CachedJob(ctx, "unique", "groq", "whisper-large-v3", "auto", false)
	if err != nil || !found || cached.Text != "Текст" || cached.DurationSeconds != 5.5 {
		t.Fatalf("cached=%#v found=%v err=%v", cached, found, err)
	}
	byMessage, err := store.JobByResultMessage(ctx, 7, 11)
	if err != nil || byMessage.ID != id {
		t.Fatalf("job=%#v err=%v", byMessage, err)
	}
	if err := store.SaveTranslation(ctx, id, "en", "model", "Text"); err != nil {
		t.Fatal(err)
	}
	translated, found, err := store.Translation(ctx, id, "en", "model")
	if err != nil || !found || translated != "Text" {
		t.Fatalf("translation=%q found=%v err=%v", translated, found, err)
	}
	if err := store.UpdateOffset(ctx, 123); err != nil {
		t.Fatal(err)
	}
	offset, err := store.Offset(ctx)
	if err != nil || offset != 123 {
		t.Fatalf("offset=%d err=%v", offset, err)
	}
	queuedID, err := store.CreateJob(ctx, Job{UserID: 42, ChatID: 7, SourceMessageID: 12, FileID: "f2", FileUniqueID: "u2", MediaKind: "voice", Provider: "groq", Model: "m", Language: "ru", Status: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	runningID, err := store.CreateJob(ctx, Job{UserID: 42, ChatID: 7, SourceMessageID: 13, FileID: "f3", FileUniqueID: "u3", MediaKind: "voice", Provider: "groq", Model: "m", Language: "ru", Status: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	if running, err := store.MarkJobRunning(ctx, runningID); err != nil || !running {
		t.Fatalf("running=%v err=%v", running, err)
	}
	recovered, err := store.RecoverPendingJobs(ctx)
	if err != nil || len(recovered) != 2 || recovered[0] != queuedID || recovered[1] != runningID {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	activeID, err := store.CreateJob(ctx, Job{UserID: 42, ChatID: 7, SourceMessageID: 14, FileID: "f4", FileUniqueID: "u4", MediaKind: "voice", Provider: "groq", Model: "m", Language: "ru", Status: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	if running, err := store.MarkJobRunning(ctx, activeID); err != nil || !running {
		t.Fatalf("running=%v err=%v", running, err)
	}
	if err := store.RequeueJob(ctx, activeID, "shutdown"); err != nil {
		t.Fatal(err)
	}
	requeued, err := store.Job(ctx, activeID)
	if err != nil || requeued.Status != "queued" || requeued.Error != "shutdown" {
		t.Fatalf("requeued=%#v err=%v", requeued, err)
	}
}

func TestPersistentAccessManagementAndLegacyImport(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "access.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := database.InitializeAccess(ctx, []int64{1}, []int64{2}); err != nil {
		t.Fatal(err)
	}
	for _, userID := range []int64{1, 2} {
		if allowed, err := database.IsAuthorized(ctx, userID); err != nil || !allowed {
			t.Fatalf("user %d allowed=%v err=%v", userID, allowed, err)
		}
	}
	if removed, err := database.RevokeUser(ctx, 2, 1); err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	// The legacy environment allowlist is not reimported after a restart.
	if err := database.InitializeAccess(ctx, []int64{1}, []int64{2}); err != nil {
		t.Fatal(err)
	}
	if allowed, err := database.IsAuthorized(ctx, 2); err != nil || allowed {
		t.Fatalf("revoked legacy user allowed=%v err=%v", allowed, err)
	}

	notify, status, err := database.RegisterAccessRequest(ctx, 3, "new_user")
	if err != nil || !notify || status != "pending" {
		t.Fatalf("notify=%v status=%q err=%v", notify, status, err)
	}
	if notify, _, err := database.RegisterAccessRequest(ctx, 3, "new_user"); err != nil || notify {
		t.Fatalf("duplicate notify=%v err=%v", notify, err)
	}
	requests, err := database.PendingAccessRequests(ctx)
	if err != nil || len(requests) != 1 || requests[0].UserID != 3 {
		t.Fatalf("requests=%#v err=%v", requests, err)
	}
	if changed, err := database.ResolveAccessRequest(ctx, 3, 1, true); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if changed, err := database.ResolveAccessRequest(ctx, 3, 1, false); err != nil || changed {
		t.Fatalf("stale decision changed=%v err=%v", changed, err)
	}
	if allowed, err := database.IsAuthorized(ctx, 3); err != nil || !allowed {
		t.Fatalf("approved user allowed=%v err=%v", allowed, err)
	}
	users, err := database.AuthorizedUsers(ctx)
	if err != nil || len(users) != 2 || users[1].Username != "new_user" {
		t.Fatalf("users=%#v err=%v", users, err)
	}
}
