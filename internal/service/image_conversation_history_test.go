package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func imageConversationHistoryFixture(id, updatedAt, title string) json.RawMessage {
	payload, _ := json.Marshal(map[string]any{
		"id":        id,
		"title":     title,
		"createdAt": "2026-07-20T10:00:00Z",
		"updatedAt": updatedAt,
		"turns":     []any{},
	})
	return payload
}

func imageConversationHistoryTitle(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	return item["title"].(string)
}

func TestImageConversationHistoryServiceSyncsAcrossInstances(t *testing.T) {
	backend := newTestStorageBackend(t)
	first := NewImageConversationHistoryService(backend)
	second := NewImageConversationHistoryService(backend)

	_, err := first.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{
		imageConversationHistoryFixture("conversation-a", "2026-07-20T10:01:00Z", "device A"),
	}})
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	loaded, err := second.List("user-a")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(loaded.Items) != 1 || imageConversationHistoryTitle(t, loaded.Items[0]) != "device A" {
		t.Fatalf("List() = %#v", loaded)
	}
	other, err := second.List("user-b")
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("List(other) = %#v, %v", other, err)
	}
}

func TestImageConversationHistoryServiceMergesUpdatesAndDeletionTombstones(t *testing.T) {
	history := NewImageConversationHistoryService(newTestStorageBackend(t))
	stale := imageConversationHistoryFixture("conversation-a", "2026-07-20T10:01:00Z", "stale")
	newer := imageConversationHistoryFixture("conversation-a", "2026-07-20T10:02:00Z", "newer")

	if _, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{stale}}); err != nil {
		t.Fatalf("initial Sync() error = %v", err)
	}
	result, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{newer}})
	if err != nil {
		t.Fatalf("update Sync() error = %v", err)
	}
	if len(result.Items) != 1 || imageConversationHistoryTitle(t, result.Items[0]) != "newer" {
		t.Fatalf("updated result = %#v", result)
	}

	result, err = history.Sync("user-a", ImageConversationHistoryDocument{
		Deletions: []HistoryDeletion{{ID: "conversation-a", DeletedAt: "2026-07-20T10:03:00Z"}},
	})
	if err != nil {
		t.Fatalf("delete Sync() error = %v", err)
	}
	if len(result.Items) != 0 || len(result.Deletions) != 1 {
		t.Fatalf("delete result = %#v", result)
	}

	result, err = history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{stale}})
	if err != nil {
		t.Fatalf("stale Sync() error = %v", err)
	}
	if len(result.Items) != 0 || len(result.Deletions) != 1 {
		t.Fatalf("stale device restored deletion: %#v", result)
	}
}

func TestImageConversationHistorySortsMixedFractionalPrecision(t *testing.T) {
	history := NewImageConversationHistoryService(newTestStorageBackend(t))
	result, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{
		imageConversationHistoryFixture("whole", "2026-07-20T10:00:00Z", "whole"),
		imageConversationHistoryFixture("fractional", "2026-07-20T10:00:00.5Z", "fractional"),
	}})
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(result.Items) != 2 || imageConversationHistoryTitle(t, result.Items[0]) != "fractional" {
		t.Fatalf("mixed-precision sort = %#v", result.Items)
	}
}

func TestImageConversationHistoryServiceMergesConcurrentTurnsInOneConversation(t *testing.T) {
	history := NewImageConversationHistoryService(newTestStorageBackend(t))
	base := imageConversationWithTurns("conversation-a", "2026-07-20T10:00:00Z", "turn-base")
	if _, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{base}}); err != nil {
		t.Fatalf("initial Sync() error = %v", err)
	}
	deviceA := imageConversationWithTurns("conversation-a", "2026-07-20T10:01:00Z", "turn-a")
	deviceB := imageConversationWithTurns("conversation-a", "2026-07-20T10:02:00Z", "turn-b")
	if _, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{deviceA}}); err != nil {
		t.Fatalf("device A Sync() error = %v", err)
	}
	result, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{deviceB}})
	if err != nil {
		t.Fatalf("device B Sync() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("merged conversations = %#v", result.Items)
	}
	var conversation struct {
		Turns []struct {
			ID string `json:"id"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(result.Items[0], &conversation); err != nil {
		t.Fatalf("decode merged conversation: %v", err)
	}
	turnIDs := map[string]bool{}
	for _, turn := range conversation.Turns {
		turnIDs[turn.ID] = true
	}
	for _, id := range []string{"turn-base", "turn-a", "turn-b"} {
		if !turnIDs[id] {
			t.Fatalf("concurrent turn %q was lost: %#v", id, conversation.Turns)
		}
	}
}

func TestImageConversationHistoryServiceAcceptsNewerTurnRegeneration(t *testing.T) {
	history := NewImageConversationHistoryService(newTestStorageBackend(t))
	success := imageConversationWithTurnState("conversation-a", "2026-07-20T10:00:00Z", "success", "img-old")
	queued := imageConversationWithTurnState("conversation-a", "2026-07-20T10:01:00Z", "queued", "img-new")
	if _, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{success}}); err != nil {
		t.Fatalf("initial Sync() error = %v", err)
	}
	result, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{queued}})
	if err != nil {
		t.Fatalf("regeneration Sync() error = %v", err)
	}
	var conversation struct {
		Turns []struct {
			Status string `json:"status"`
			Images []struct {
				ID string `json:"id"`
			} `json:"images"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(result.Items[0], &conversation); err != nil {
		t.Fatalf("decode regenerated conversation: %v", err)
	}
	if len(conversation.Turns) != 1 || conversation.Turns[0].Status != "queued" || conversation.Turns[0].Images[0].ID != "img-new" {
		t.Fatalf("older terminal turn won over regeneration: %#v", conversation.Turns)
	}
}

func TestImageConversationHistoryServicePreservesInlineImageWithoutDurableURL(t *testing.T) {
	history := NewImageConversationHistoryService(newTestStorageBackend(t))
	inline := strings.Repeat("A", 20*1024)
	payload, err := json.Marshal(map[string]any{
		"id": "conversation-inline", "title": "inline", "createdAt": "2026-07-20T10:00:00Z", "updatedAt": "2026-07-20T10:01:00Z",
		"turns": []any{map[string]any{
			"id": "turn-1", "status": "success", "createdAt": "2026-07-20T10:00:00Z",
			"images": []any{map[string]any{"id": "image-1", "status": "success", "b64_json": inline}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal inline payload: %v", err)
	}
	result, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{payload}})
	if err != nil {
		t.Fatalf("inline Sync() error = %v", err)
	}
	if !containsSubstring(string(result.Items[0]), `"b64_json":"`+inline+`"`) {
		t.Fatalf("inline image payload was removed: %s", result.Items[0])
	}
}

func imageConversationWithTurns(id, updatedAt string, turnIDs ...string) json.RawMessage {
	turns := make([]map[string]any, 0, len(turnIDs))
	for index, turnID := range turnIDs {
		turns = append(turns, map[string]any{
			"id": turnID, "prompt": turnID, "createdAt": "2026-07-20T10:00:00Z",
			"updatedAt": updatedAt, "status": "success", "images": []any{}, "mode": "generate",
			"index": index,
		})
	}
	payload, _ := json.Marshal(map[string]any{
		"id": id, "title": id, "createdAt": "2026-07-20T10:00:00Z", "updatedAt": updatedAt, "turns": turns,
	})
	return payload
}

func imageConversationWithTurnState(id, updatedAt, status, imageID string) json.RawMessage {
	payload, _ := json.Marshal(map[string]any{
		"id": id, "title": id, "createdAt": "2026-07-20T10:00:00Z", "updatedAt": updatedAt,
		"turns": []any{map[string]any{
			"id": "turn-1", "prompt": "regenerate", "createdAt": "2026-07-20T10:00:00Z", "updatedAt": updatedAt, "status": status,
			"images": []any{map[string]any{"id": imageID, "status": map[string]string{"success": "success", "queued": "loading"}[status]}},
		}},
	})
	return payload
}

func TestImageConversationHistoryServiceMergesEachTurnByItsOwnVersion(t *testing.T) {
	history := NewImageConversationHistoryService(newTestStorageBackend(t))
	completed := imageConversationWithTurnState("conversation-a", "2026-07-20T10:02:00Z", "success", "img-finished")
	staleWithNewTurn, _ := json.Marshal(map[string]any{
		"id": "conversation-a", "title": "new turn", "createdAt": "2026-07-20T10:00:00Z", "updatedAt": "2026-07-20T10:03:00Z",
		"turns": []any{
			map[string]any{
				"id": "turn-1", "prompt": "old copy", "createdAt": "2026-07-20T10:00:00Z", "updatedAt": "2026-07-20T10:01:00Z", "status": "generating",
				"images": []any{map[string]any{"id": "img-stale", "status": "loading"}},
			},
			map[string]any{
				"id": "turn-2", "prompt": "new work", "createdAt": "2026-07-20T10:03:00Z", "updatedAt": "2026-07-20T10:03:00Z", "status": "queued",
				"images": []any{map[string]any{"id": "img-new", "status": "loading"}},
			},
		},
	})
	if _, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{completed}}); err != nil {
		t.Fatalf("completed Sync() error = %v", err)
	}
	result, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{staleWithNewTurn}})
	if err != nil {
		t.Fatalf("stale + new turn Sync() error = %v", err)
	}
	var conversation struct {
		Turns []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Images []struct {
				ID string `json:"id"`
			} `json:"images"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(result.Items[0], &conversation); err != nil {
		t.Fatalf("decode merged conversation: %v", err)
	}
	if len(conversation.Turns) != 2 {
		t.Fatalf("merged turns = %#v", conversation.Turns)
	}
	if conversation.Turns[0].ID != "turn-1" || conversation.Turns[0].Status != "success" || conversation.Turns[0].Images[0].ID != "img-finished" {
		t.Fatalf("stale turn overwrote completed turn: %#v", conversation.Turns[0])
	}
	if conversation.Turns[1].ID != "turn-2" {
		t.Fatalf("new concurrent turn was lost: %#v", conversation.Turns)
	}
}

func TestImageConversationHistoryStripsHeavyInlinePayloads(t *testing.T) {
	history := NewImageConversationHistoryService(newTestStorageBackend(t))
	heavy := make([]byte, 20*1024)
	for i := range heavy {
		heavy[i] = 'A'
	}
	payload, err := json.Marshal(map[string]any{
		"id":        "conversation-heavy",
		"title":     "heavy",
		"createdAt": "2026-07-20T10:00:00Z",
		"updatedAt": "2026-07-20T10:04:00Z",
		"turns": []any{
			map[string]any{
				"id":     "turn-1",
				"prompt": "edit this",
				"referenceImages": []any{
					map[string]any{
						"name":    "ref.png",
						"type":    "image/png",
						"dataUrl": "data:image/png;base64," + string(heavy),
						"url":     "/images/2026/07/20/ref.png",
						"path":    "2026/07/20/ref.png",
					},
				},
				"images": []any{
					map[string]any{
						"id":       "img-1",
						"status":   "success",
						"url":      "/images/2026/07/20/out.png",
						"path":     "2026/07/20/out.png",
						"b64_json": string(heavy),
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	result, err := history.Sync("user-a", ImageConversationHistoryDocument{Items: []json.RawMessage{payload}})
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("Sync() items = %d", len(result.Items))
	}
	raw := string(result.Items[0])
	if len(result.Items[0]) > 4*1024 {
		t.Fatalf("expected stripped payload under 4KB, got %d bytes", len(result.Items[0]))
	}
	if containsSubstring(raw, "data:image/png;base64,") {
		t.Fatalf("dataUrl should be stripped when url/path exists: %s", raw)
	}
	if containsSubstring(raw, `"b64_json"`) {
		t.Fatalf("b64_json should be stripped when url/path exists: %s", raw)
	}
	if !containsSubstring(raw, `/images/2026/07/20/out.png`) || !containsSubstring(raw, `/images/2026/07/20/ref.png`) {
		t.Fatalf("durable urls should remain: %s", raw)
	}
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
