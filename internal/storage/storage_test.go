package storage

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseBackendStoresDocumentsAndLogs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chatgpt2api.db")
	backend, err := NewDatabaseBackend("sqlite:///" + filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("NewDatabaseBackend() error = %v", err)
	}
	defer backend.db.Close()

	if err := backend.SaveAccounts([]map[string]any{{"access_token": "token-1", "type": "Plus"}}); err != nil {
		t.Fatalf("SaveAccounts() error = %v", err)
	}
	if err := backend.SaveAuthKeys([]map[string]any{{"id": "key-1", "key": "sk-test"}}); err != nil {
		t.Fatalf("SaveAuthKeys() error = %v", err)
	}
	if err := backend.SaveJSONDocument("announcements.json", []map[string]any{{"id": "a1", "content": "hello"}}); err != nil {
		t.Fatalf("SaveJSONDocument() error = %v", err)
	}
	if err := backend.AppendLog(map[string]any{
		"time":    "2026-04-30 10:00:00",
		"type":    "event",
		"summary": "ok",
		"detail":  map[string]any{"status": "success"},
	}); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := backend.AppendLog(map[string]any{
		"time":    "2026-04-29 10:00:00",
		"type":    "event",
		"summary": "skip",
	}); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	accounts, err := backend.LoadAccounts()
	if err != nil {
		t.Fatalf("LoadAccounts() error = %v", err)
	}
	if len(accounts) != 1 || accounts[0]["access_token"] != "token-1" {
		t.Fatalf("LoadAccounts() = %#v", accounts)
	}

	doc, err := backend.LoadJSONDocument("announcements.json")
	if err != nil {
		t.Fatalf("LoadJSONDocument() error = %v", err)
	}
	items, ok := doc.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("LoadJSONDocument() = %#v", doc)
	}

	logs, err := backend.QueryLogs("2026-04-30", "2026-04-30", 10)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0]["summary"] != "ok" {
		t.Fatalf("QueryLogs() = %#v", logs)
	}

	health := backend.HealthCheck()
	if health["document_count"] != 1 || health["log_count"] != 2 {
		t.Fatalf("HealthCheck() = %#v", health)
	}
}

func TestDatabaseBackendStoresExternalImageTasksIndependently(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chatgpt2api.db")
	backend, err := NewDatabaseBackend("sqlite:///" + filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("NewDatabaseBackend() error = %v", err)
	}
	defer backend.Close()
	if err := backend.UpsertExternalImageTask("alice:invalid", map[string]any{"owner_id": "alice"}); err == nil {
		t.Fatal("UpsertExternalImageTask() accepted a task without updated_at")
	}
	if err := backend.UpsertExternalImageTask("alice:invalid", map[string]any{"updated_at": "2026-07-22T08:00:00Z"}); err == nil {
		t.Fatal("UpsertExternalImageTask() accepted a task without owner_id")
	}

	now := "2026-07-22T08:00:00Z"
	first := map[string]any{"id": "first", "owner_id": "alice", "status": "queued", "updated_at": now}
	second := map[string]any{"id": "second", "owner_id": "alice", "status": "success", "updated_at": now, "marker": "unchanged"}
	if err := backend.UpsertExternalImageTask("alice:first", first); err != nil {
		t.Fatalf("UpsertExternalImageTask(first) error = %v", err)
	}
	if err := backend.UpsertExternalImageTask("alice:second", second); err != nil {
		t.Fatalf("UpsertExternalImageTask(second) error = %v", err)
	}

	first["status"] = "running"
	first["updated_at"] = "2026-07-22T08:01:00Z"
	if err := backend.UpsertExternalImageTask("alice:first", first); err != nil {
		t.Fatalf("UpsertExternalImageTask(update first) error = %v", err)
	}
	items, err := backend.LoadExternalImageTasks()
	if err != nil {
		t.Fatalf("LoadExternalImageTasks() error = %v", err)
	}
	byID := make(map[string]map[string]any, len(items))
	for _, item := range items {
		byID[item["id"].(string)] = item
	}
	if len(byID) != 2 || byID["first"]["status"] != "running" || byID["second"]["marker"] != "unchanged" {
		t.Fatalf("external image task records = %#v", items)
	}

	if err := backend.DeleteExternalImageTasks([]string{"alice:first", "alice:second"}); err != nil {
		t.Fatalf("DeleteExternalImageTasks() error = %v", err)
	}
	items, err = backend.LoadExternalImageTasks()
	if err != nil || len(items) != 0 {
		t.Fatalf("tasks after batch delete = %#v, %v", items, err)
	}
}

func TestDatabaseBackendRejectsConcurrentSQLiteInstance(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chatgpt2api.db")
	dsn := "sqlite:///" + filepath.ToSlash(dbPath)
	first, err := NewDatabaseBackend(dsn)
	if err != nil {
		t.Fatalf("first NewDatabaseBackend() error = %v", err)
	}
	defer first.Close()
	second, err := NewDatabaseBackend(dsn)
	if second != nil {
		_ = second.Close()
	}
	if err == nil {
		t.Fatal("second database instance acquired the same SQLite database")
	}
}

func TestDatabaseInstanceLockNameIgnoresMySQLCredentials(t *testing.T) {
	first := databaseInstanceLockName("mysql", "alice:secret-a@tcp(db.example:3306)/chatgpt2api?parseTime=true")
	second := databaseInstanceLockName("mysql", "bob:secret-b@tcp(db.example:3306)/chatgpt2api?parseTime=true&timeout=5s")
	if first != second {
		t.Fatalf("same MySQL database produced different lock names: %q != %q", first, second)
	}
	otherDatabase := databaseInstanceLockName("mysql", "alice:secret-a@tcp(db.example:3306)/other?parseTime=true")
	if first == otherDatabase {
		t.Fatalf("different MySQL databases produced the same lock name: %q", first)
	}
	if strings.Contains(first, "alice") || strings.Contains(first, "secret-a") {
		t.Fatalf("lock name exposed credentials: %q", first)
	}
}

func TestSQLitePoolDoesNotRotateLockedConnection(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chatgpt2api.db")
	backend, err := NewDatabaseBackend("sqlite:///" + filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("NewDatabaseBackend() error = %v", err)
	}
	defer backend.Close()

	before := backend.db.Stats().MaxLifetimeClosed
	for range 3 {
		if err := backend.db.Ping(); err != nil {
			t.Fatalf("Ping() error = %v", err)
		}
	}
	if after := backend.db.Stats().MaxLifetimeClosed; after != before {
		t.Fatalf("SQLite locked connection rotated: before=%d after=%d", before, after)
	}
}

func TestDatabaseBackendDoesNotSkipCorruptRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chatgpt2api.db")
	backend, err := NewDatabaseBackend("sqlite:///" + filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if _, err := backend.db.Exec(`INSERT INTO accounts (access_token, data) VALUES (?, ?)`, "broken", "not-json"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.LoadAccounts(); err == nil {
		t.Fatal("LoadAccounts() accepted corrupt JSON row")
	}
	if err := backend.SaveAccounts([]map[string]any{{"access_token": "bad", "value": make(chan int)}}); err == nil {
		t.Fatal("SaveAccounts() skipped an unserializable row")
	}
}

func TestDatabaseBackendQueryLogsEmptyReturnsJSONArray(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chatgpt2api.db")
	backend, err := NewDatabaseBackend("sqlite:///" + filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("NewDatabaseBackend() error = %v", err)
	}
	defer backend.db.Close()

	logs, err := backend.QueryLogs("2026-04-30", "2026-04-30", 10)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if logs == nil {
		t.Fatal("QueryLogs() returned nil slice, want empty slice")
	}
	data, err := json.Marshal(map[string]any{"items": logs})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(data) != `{"items":[]}` {
		t.Fatalf("marshaled logs = %s, want {\"items\":[]}", data)
	}
}

func TestDatabaseBackendDeletesLogsBeforeDay(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chatgpt2api.db")
	backend, err := NewDatabaseBackend("sqlite:///" + filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("NewDatabaseBackend() error = %v", err)
	}
	defer backend.db.Close()

	for _, item := range []map[string]any{
		{"time": "2026-04-28 10:00:00", "type": "event", "summary": "old"},
		{"time": "2026-04-29 10:00:00", "type": "event", "summary": "cutoff"},
		{"time": "2026-04-30 10:00:00", "type": "event", "summary": "new"},
	} {
		if err := backend.AppendLog(item); err != nil {
			t.Fatalf("AppendLog() error = %v", err)
		}
	}

	deleted, err := backend.DeleteLogsBefore("2026-04-29")
	if err != nil {
		t.Fatalf("DeleteLogsBefore() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteLogsBefore() deleted = %d, want 1", deleted)
	}
	logs, err := backend.QueryLogs("", "", 0)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("remaining logs = %#v, want 2", logs)
	}
}

func TestNewBackendFromEnvDefaultsToSQLiteProjectDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STORAGE_BACKEND", "")
	t.Setenv("DATABASE_URL", "")

	backend, err := NewBackendFromEnv(dir)
	if err != nil {
		t.Fatalf("NewBackendFromEnv() error = %v", err)
	}
	database, ok := backend.(*DatabaseBackend)
	if !ok {
		t.Fatalf("NewBackendFromEnv() returned %T, want *DatabaseBackend", backend)
	}
	defer database.db.Close()
	if database.driver != "sqlite" {
		t.Fatalf("driver = %q, want sqlite", database.driver)
	}
	want := filepath.ToSlash(filepath.Join(dir, "chatgpt2api.db"))
	if database.dsn != want {
		t.Fatalf("dsn = %q, want %q", database.dsn, want)
	}
}

func TestNewBackendFromEnvRejectsJSONBackend(t *testing.T) {
	t.Setenv("STORAGE_BACKEND", "json")
	t.Setenv("DATABASE_URL", "")

	_, err := NewBackendFromEnv(t.TempDir())
	if err == nil {
		t.Fatal("NewBackendFromEnv() succeeded, want error")
	}
	if !strings.Contains(err.Error(), "unknown storage backend: json") {
		t.Fatalf("NewBackendFromEnv() error = %v", err)
	}
}

func TestDocumentNameValidation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chatgpt2api.db")
	backend, err := NewDatabaseBackend("sqlite:///" + filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("NewDatabaseBackend() error = %v", err)
	}
	defer backend.db.Close()

	for _, name := range []string{"../x.json", "/x.json", "a/../x.json", "C:/x.json"} {
		t.Run(name, func(t *testing.T) {
			if err := backend.SaveJSONDocument(name, map[string]any{}); err == nil {
				t.Fatalf("SaveJSONDocument(%q) succeeded, want error", name)
			}
		})
	}
}
