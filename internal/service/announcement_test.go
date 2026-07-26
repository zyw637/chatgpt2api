package service

import "testing"

func TestAnnouncementMutationsRollbackOnPersistenceFailure(t *testing.T) {
	backend := newFailingStorageBackend(t)
	service := NewAnnouncementService(backend)
	backend.failDocument = "announcements.json"

	if item, err := service.Create(map[string]any{"content": "hello"}); err == nil || item != nil {
		t.Fatalf("Create() = %#v, %v", item, err)
	}
	if items := service.ListAll(); len(items) != 0 {
		t.Fatalf("failed create changed in-memory state: %#v", items)
	}
}
