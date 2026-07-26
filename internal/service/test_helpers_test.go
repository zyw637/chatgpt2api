package service

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"chatgpt2api/internal/storage"
)

type failingStorageBackend struct {
	storage.Backend
	failAccounts       bool
	failAccountsLoad   bool
	failAuth           bool
	failAuthLoad       bool
	failDocument       string
	failDocumentLoad   string
	failAllDocument    bool
	failExternalTask   bool
	failExternalLoad   bool
	failExternalDelete bool
}

func (b *failingStorageBackend) LoadAccounts() ([]map[string]any, error) {
	if b.failAccountsLoad {
		return nil, errors.New("forced accounts load failure")
	}
	return b.Backend.LoadAccounts()
}

func newFailingStorageBackend(t *testing.T) *failingStorageBackend {
	t.Helper()
	return &failingStorageBackend{Backend: newTestStorageBackend(t)}
}

func (b *failingStorageBackend) SaveAccounts(items []map[string]any) error {
	if b.failAccounts {
		return errors.New("forced accounts save failure")
	}
	return b.Backend.SaveAccounts(items)
}

func (b *failingStorageBackend) SaveAuthKeys(items []map[string]any) error {
	if b.failAuth {
		return errors.New("forced auth save failure")
	}
	return b.Backend.SaveAuthKeys(items)
}

func (b *failingStorageBackend) LoadAuthKeys() ([]map[string]any, error) {
	if b.failAuthLoad {
		return nil, errors.New("forced auth load failure")
	}
	return b.Backend.LoadAuthKeys()
}

func (b *failingStorageBackend) LoadJSONDocument(name string) (any, error) {
	if b.failDocumentLoad == name {
		return nil, errors.New("forced document load failure")
	}
	return b.Backend.(storage.JSONDocumentBackend).LoadJSONDocument(name)
}

func (b *failingStorageBackend) SaveJSONDocument(name string, value any) error {
	if b.failAllDocument || b.failDocument == name {
		return errors.New("forced document save failure")
	}
	return b.Backend.(storage.JSONDocumentBackend).SaveJSONDocument(name, value)
}

func (b *failingStorageBackend) DeleteJSONDocument(name string) error {
	return b.Backend.(storage.JSONDocumentBackend).DeleteJSONDocument(name)
}

func (b *failingStorageBackend) LoadExternalImageTasks() (map[string]map[string]any, error) {
	if b.failExternalLoad {
		return nil, errors.New("forced external image task load failure")
	}
	return b.Backend.(storage.ExternalImageTaskBackend).LoadExternalImageTasks()
}

func (b *failingStorageBackend) UpsertExternalImageTask(taskKey string, task map[string]any) error {
	if b.failExternalTask {
		return errors.New("forced external image task save failure")
	}
	return b.Backend.(storage.ExternalImageTaskBackend).UpsertExternalImageTask(taskKey, task)
}

func (b *failingStorageBackend) DeleteExternalImageTasks(taskKeys []string) error {
	if b.failExternalDelete {
		return errors.New("forced external image task delete failure")
	}
	return b.Backend.(storage.ExternalImageTaskBackend).DeleteExternalImageTasks(taskKeys)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestStorageBackend(t *testing.T) storage.Backend {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "chatgpt2api.db")
	backend, err := storage.NewDatabaseBackend("sqlite:///" + filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("NewDatabaseBackend() error = %v", err)
	}
	t.Cleanup(func() {
		_ = backend.Close()
	})
	return backend
}
