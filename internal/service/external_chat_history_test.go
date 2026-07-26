package service

import (
	"strings"
	"testing"
	"time"

	"chatgpt2api/internal/storage"
)

func TestExternalChatHistoryServicePersistsAndIsolatesOwners(t *testing.T) {
	backend := newTestStorageBackend(t)
	history := NewExternalChatHistoryService(backend)
	conversation := externalChatHistoryTestConversation("chat-a", "2026-07-18T12:00:00Z")

	saved, err := history.Sync("user-alice", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{conversation}})
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(saved.Items) != 1 || saved.Items[0].Messages[0].Content != "hello" {
		t.Fatalf("Replace() = %#v", saved)
	}
	saved.Items[0].Messages[0].Content = "mutated"

	reloaded, err := NewExternalChatHistoryService(backend).List("user-alice")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(reloaded.Items) != 1 || reloaded.Items[0].Messages[0].Content != "hello" {
		t.Fatalf("List() = %#v", reloaded)
	}
	other, err := history.List("user-bob")
	if err != nil {
		t.Fatalf("List(other) error = %v", err)
	}
	if len(other.Items) != 0 {
		t.Fatalf("List(other) = %#v", other)
	}

	documentStore := backend.(storage.JSONDocumentBackend)
	documentName := externalChatHistoryDocumentName("user-alice")
	if strings.Contains(documentName, "user-alice") {
		t.Fatalf("document name exposes owner: %q", documentName)
	}
	if raw, err := documentStore.LoadJSONDocument(documentName); err != nil || raw == nil {
		t.Fatalf("LoadJSONDocument() = %#v, %v", raw, err)
	}
}

func TestExternalChatHistoryServiceAppliesConversationAndMessageLimits(t *testing.T) {
	history := NewExternalChatHistoryService(newTestStorageBackend(t))
	items := make([]ExternalChatHistoryConversation, 0, maxExternalChatConversations+1)
	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	for index := 0; index <= maxExternalChatConversations; index++ {
		conversation := externalChatHistoryTestConversation(
			"chat-"+time.Duration(index).String(),
			base.Add(time.Duration(index)*time.Minute).Format(time.RFC3339Nano),
		)
		items = append(items, conversation)
	}
	messages := make([]ExternalChatHistoryMessage, 0, maxExternalChatMessages+1)
	for index := 0; index <= maxExternalChatMessages; index++ {
		role := "user"
		if index%2 == 0 {
			role = "assistant"
		}
		messages = append(messages, ExternalChatHistoryMessage{
			ID:        "message-" + time.Duration(index).String(),
			Role:      role,
			Content:   "content",
			CreatedAt: base.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
			Status:    "complete",
		})
	}
	items[len(items)-1].Messages = messages

	saved, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: items})
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(saved.Items) != maxExternalChatConversations {
		t.Fatalf("conversation count = %d", len(saved.Items))
	}
	if saved.Items[0].ID != items[len(items)-1].ID {
		t.Fatalf("newest conversation = %q", saved.Items[0].ID)
	}
	if len(saved.Items[0].Messages) > maxExternalChatMessages || saved.Items[0].Messages[0].Role == "assistant" {
		t.Fatalf("limited messages = %#v", saved.Items[0].Messages)
	}
}

func TestExternalChatHistoryServiceRejectsInvalidInput(t *testing.T) {
	history := NewExternalChatHistoryService(newTestStorageBackend(t))
	tests := []struct {
		name   string
		mutate func(*ExternalChatHistoryConversation)
	}{
		{name: "missing conversation id", mutate: func(item *ExternalChatHistoryConversation) { item.ID = "" }},
		{name: "invalid role", mutate: func(item *ExternalChatHistoryConversation) { item.Messages[0].Role = "system" }},
		{name: "invalid status", mutate: func(item *ExternalChatHistoryConversation) { item.Messages[0].Status = "pending" }},
		{name: "invalid temperature", mutate: func(item *ExternalChatHistoryConversation) { item.Temperature = "3" }},
		{name: "invalid token limit", mutate: func(item *ExternalChatHistoryConversation) { item.MaxCompletionTokens = "0" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := externalChatHistoryTestConversation("chat-a", "2026-07-18T12:00:00Z")
			test.mutate(&item)
			if _, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{item}}); err == nil {
				t.Fatal("Sync() succeeded")
			}
		})
	}
}

func TestExternalChatHistoryServiceDeletionSurvivesStaleDeviceSync(t *testing.T) {
	history := NewExternalChatHistoryService(newTestStorageBackend(t))
	items := []ExternalChatHistoryConversation{
		externalChatHistoryTestConversation("chat-a", "2026-07-18T12:00:00Z"),
		externalChatHistoryTestConversation("chat-b", "2026-07-18T12:01:00Z"),
		externalChatHistoryTestConversation("chat-c", "2026-07-18T12:02:00Z"),
	}
	if _, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: items}); err != nil {
		t.Fatalf("initial Sync() error = %v", err)
	}
	if _, err := history.Sync("user-1", ExternalChatHistoryDocument{
		Items: []ExternalChatHistoryConversation{items[0], items[2]},
		Deletions: []ExternalChatHistoryDeletion{{
			ID:        "chat-b",
			DeletedAt: "2026-07-19T12:00:00Z",
		}},
	}); err != nil {
		t.Fatalf("delete Sync() error = %v", err)
	}

	result, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: items})
	if err != nil {
		t.Fatalf("stale Sync() error = %v", err)
	}
	if len(result.Items) != 2 || result.Items[0].ID == "chat-b" || result.Items[1].ID == "chat-b" {
		t.Fatalf("stale device restored deletion: %#v", result)
	}
	if len(result.Deletions) != 1 || result.Deletions[0].ID != "chat-b" {
		t.Fatalf("deletions = %#v", result.Deletions)
	}
}

func TestExternalChatHistoryServiceMergesConcurrentConversationUpdates(t *testing.T) {
	history := NewExternalChatHistoryService(newTestStorageBackend(t))
	chatA := externalChatHistoryTestConversation("chat-a", "2026-07-18T12:00:00Z")
	chatB := externalChatHistoryTestConversation("chat-b", "2026-07-18T12:00:00Z")
	if _, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{chatA, chatB}}); err != nil {
		t.Fatalf("initial Sync() error = %v", err)
	}
	chatA.Title = "updated on device A"
	chatA.UpdatedAt = "2026-07-18T12:05:00Z"
	if _, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{chatA, chatB}}); err != nil {
		t.Fatalf("device A Sync() error = %v", err)
	}
	chatB.Title = "updated on device B"
	chatB.UpdatedAt = "2026-07-18T12:06:00Z"
	result, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{
		externalChatHistoryTestConversation("chat-a", "2026-07-18T12:00:00Z"),
		chatB,
	}})
	if err != nil {
		t.Fatalf("device B Sync() error = %v", err)
	}
	titles := map[string]string{}
	for _, item := range result.Items {
		titles[item.ID] = item.Title
	}
	if titles["chat-a"] != "updated on device A" || titles["chat-b"] != "updated on device B" {
		t.Fatalf("merged titles = %#v", titles)
	}
}

func TestExternalChatHistoryServiceMergesConcurrentMessagesInOneConversation(t *testing.T) {
	history := NewExternalChatHistoryService(newTestStorageBackend(t))
	base := externalChatHistoryTestConversation("chat-a", "2026-07-18T12:00:00Z")
	if _, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{base}}); err != nil {
		t.Fatalf("initial Sync() error = %v", err)
	}
	deviceA := base
	deviceA.UpdatedAt = "2026-07-18T12:01:00Z"
	deviceA.Messages = append(deviceA.Messages, ExternalChatHistoryMessage{
		ID: "message-a", Role: "user", Content: "from device A", CreatedAt: "2026-07-18T12:01:00Z", Status: "complete",
	})
	if _, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{deviceA}}); err != nil {
		t.Fatalf("device A Sync() error = %v", err)
	}
	deviceB := base
	deviceB.UpdatedAt = "2026-07-18T12:02:00Z"
	deviceB.Messages = append(deviceB.Messages, ExternalChatHistoryMessage{
		ID: "message-b", Role: "user", Content: "from device B", CreatedAt: "2026-07-18T12:02:00Z", Status: "complete",
	})
	result, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{deviceB}})
	if err != nil {
		t.Fatalf("device B Sync() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("merged conversations = %#v", result.Items)
	}
	messageIDs := map[string]bool{}
	for _, message := range result.Items[0].Messages {
		messageIDs[message.ID] = true
	}
	if !messageIDs["message-a"] || !messageIDs["message-b"] || !messageIDs["message-1"] {
		t.Fatalf("concurrent messages were lost: %#v", result.Items[0].Messages)
	}
}

func TestExternalChatHistoryServiceMessageDeletionBlocksStaleMessage(t *testing.T) {
	history := NewExternalChatHistoryService(newTestStorageBackend(t))
	base := externalChatHistoryTestConversation("chat-a", "2026-07-18T12:00:00Z")
	if _, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{base}}); err != nil {
		t.Fatalf("initial Sync() error = %v", err)
	}
	deleted := base
	deleted.UpdatedAt = "2026-07-18T12:03:00Z"
	deleted.Messages = nil
	deleted.MessageDeletions = []HistoryDeletion{{ID: "message-1", DeletedAt: "2026-07-18T12:03:00Z"}}
	if _, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{deleted}}); err != nil {
		t.Fatalf("delete Sync() error = %v", err)
	}
	result, err := history.Sync("user-1", ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{base}})
	if err != nil {
		t.Fatalf("stale Sync() error = %v", err)
	}
	if len(result.Items) != 1 || len(result.Items[0].Messages) != 0 {
		t.Fatalf("stale message was restored: %#v", result)
	}
	if len(result.Items[0].MessageDeletions) != 1 || result.Items[0].MessageDeletions[0].ID != "message-1" {
		t.Fatalf("message tombstone was lost: %#v", result.Items[0].MessageDeletions)
	}
}

func externalChatHistoryTestConversation(id, updatedAt string) ExternalChatHistoryConversation {
	return ExternalChatHistoryConversation{
		ID:                  id,
		Title:               "Greeting",
		ProviderID:          "provider-1",
		Model:               "model-1",
		SystemPrompt:        "Be concise",
		Temperature:         "0.7",
		MaxCompletionTokens: "1024",
		CreatedAt:           "2026-07-18T11:00:00Z",
		UpdatedAt:           updatedAt,
		Messages: []ExternalChatHistoryMessage{{
			ID:        "message-1",
			Role:      "user",
			Content:   "hello",
			CreatedAt: "2026-07-18T11:00:00Z",
			Status:    "complete",
		}},
	}
}
