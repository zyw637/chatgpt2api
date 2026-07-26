package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"chatgpt2api/internal/storage"
	"chatgpt2api/internal/util"
)

const (
	imageConversationHistoryDocumentDir = "image_conversations"
	maxImageConversationHistoryItems    = 50
	maxImageConversationHistoryDeletes  = 500
)

// ImageConversationHistoryDocument intentionally keeps conversation payloads opaque.
// The frontend owns the conversation schema and can add fields without a backend migration.
type ImageConversationHistoryDocument struct {
	Items     []json.RawMessage `json:"items"`
	Deletions []HistoryDeletion `json:"deletions"`
}

type ImageConversationHistoryService struct {
	mu    sync.Mutex
	store storage.JSONDocumentBackend
}

func NewImageConversationHistoryService(backend ...storage.Backend) *ImageConversationHistoryService {
	return &ImageConversationHistoryService{store: firstJSONDocumentStore(backend)}
}

func (s *ImageConversationHistoryService) List(ownerID string) (ImageConversationHistoryDocument, error) {
	ownerID = util.Clean(ownerID)
	if ownerID == "" {
		return ImageConversationHistoryDocument{}, fmt.Errorf("owner_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(ownerID)
}

func (s *ImageConversationHistoryService) Sync(ownerID string, incoming ImageConversationHistoryDocument) (ImageConversationHistoryDocument, error) {
	ownerID = util.Clean(ownerID)
	if ownerID == "" {
		return ImageConversationHistoryDocument{}, fmt.Errorf("owner_id is required")
	}
	normalized, err := normalizeImageConversationHistoryDocument(incoming)
	if err != nil {
		return ImageConversationHistoryDocument{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.loadLocked(ownerID)
	if err != nil {
		return ImageConversationHistoryDocument{}, err
	}
	merged := mergeImageConversationHistory(stored, normalized)
	if err := saveStoredJSON(s.store, imageConversationHistoryDocumentName(ownerID), merged); err != nil {
		return ImageConversationHistoryDocument{}, err
	}
	return cloneImageConversationHistoryDocument(merged), nil
}

func (s *ImageConversationHistoryService) loadLocked(ownerID string) (ImageConversationHistoryDocument, error) {
	if s.store == nil {
		return ImageConversationHistoryDocument{}, fmt.Errorf("storage document backend is required")
	}
	raw, err := s.store.LoadJSONDocument(imageConversationHistoryDocumentName(ownerID))
	if err != nil {
		return ImageConversationHistoryDocument{}, err
	}
	if raw == nil {
		return ImageConversationHistoryDocument{Items: []json.RawMessage{}, Deletions: []HistoryDeletion{}}, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ImageConversationHistoryDocument{}, fmt.Errorf("encode image conversation history: %w", err)
	}
	var document ImageConversationHistoryDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		return ImageConversationHistoryDocument{}, fmt.Errorf("decode image conversation history: %w", err)
	}
	return normalizeImageConversationHistoryDocument(document)
}

func imageConversationHistoryDocumentName(ownerID string) string {
	return imageConversationHistoryDocumentDir + "/" + util.SHA256Hex(ownerID) + ".json"
}

type imageConversationHistoryItem struct {
	id        string
	updatedAt string
	raw       json.RawMessage
}

func normalizeImageConversationHistoryDocument(document ImageConversationHistoryDocument) (ImageConversationHistoryDocument, error) {
	now := util.NowISO()
	items := make([]imageConversationHistoryItem, 0, len(document.Items))
	seen := make(map[string]struct{}, len(document.Items))
	for index, raw := range document.Items {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			return ImageConversationHistoryDocument{}, fmt.Errorf("conversation %d: invalid JSON", index+1)
		}
		id := strings.TrimSpace(util.Clean(item["id"]))
		if id == "" {
			return ImageConversationHistoryDocument{}, fmt.Errorf("conversation %d: id is required", index+1)
		}
		if _, ok := seen[id]; ok {
			return ImageConversationHistoryDocument{}, fmt.Errorf("conversation %d: duplicate id", index+1)
		}
		seen[id] = struct{}{}
		updatedAt, err := normalizeHistoryInputTime(util.Clean(item["updatedAt"]), now)
		if err != nil {
			return ImageConversationHistoryDocument{}, fmt.Errorf("conversation %d: updatedAt: %w", index+1, err)
		}
		item["id"] = id
		item["updatedAt"] = updatedAt
		for turnIndex, turn := range util.AsMapSlice(item["turns"]) {
			turnUpdatedAt, err := normalizeHistoryInputTime(util.Clean(turn["updatedAt"]), util.Clean(turn["createdAt"]))
			if err != nil {
				return ImageConversationHistoryDocument{}, fmt.Errorf("conversation %d turn %d: updatedAt: %w", index+1, turnIndex+1, err)
			}
			turn["updatedAt"] = turnUpdatedAt
		}
		// Drop inline base64 blobs that make GET/PUT /api/image-conversations multi-megabyte and slow.
		stripHeavyImageConversationFields(item)
		encoded, err := json.Marshal(item)
		if err != nil {
			return ImageConversationHistoryDocument{}, fmt.Errorf("conversation %d: encode failed", index+1)
		}
		items = append(items, imageConversationHistoryItem{id: id, updatedAt: updatedAt, raw: encoded})
	}
	sort.SliceStable(items, func(i, j int) bool { return historyTimeAfter(items[i].updatedAt, items[j].updatedAt) })
	if len(items) > maxImageConversationHistoryItems {
		items = items[:maxImageConversationHistoryItems]
	}
	deletions, err := NormalizeHistoryDeletions(document.Deletions, maxImageConversationHistoryDeletes)
	if err != nil {
		return ImageConversationHistoryDocument{}, err
	}
	return resolveImageConversationHistoryDocument(items, deletions), nil
}

func mergeImageConversationHistory(left, right ImageConversationHistoryDocument) ImageConversationHistoryDocument {
	all := append(append([]json.RawMessage{}, left.Items...), right.Items...)
	items := make(map[string]imageConversationHistoryItem, len(all))
	for _, raw := range all {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		id := strings.TrimSpace(util.Clean(item["id"]))
		if id == "" {
			continue
		}
		updatedAt := NormalizeHistoryTime(util.Clean(item["updatedAt"]), util.NowISO())
		candidate := imageConversationHistoryItem{id: id, updatedAt: updatedAt, raw: raw}
		current, ok := items[id]
		if !ok {
			items[id] = candidate
			continue
		}
		items[id] = mergeImageConversationHistoryItem(current, candidate)
	}
	deletionMap := HistoryDeletionMap(MergeHistoryDeletions(left.Deletions, right.Deletions))
	normalizedDeletes := make([]HistoryDeletion, 0, len(deletionMap))
	for _, deletion := range deletionMap {
		normalizedDeletes = append(normalizedDeletes, deletion)
	}
	values := make([]imageConversationHistoryItem, 0, len(items))
	for _, item := range items {
		values = append(values, item)
	}
	return resolveImageConversationHistoryDocument(values, normalizedDeletes)
}

func mergeImageConversationHistoryItem(current, candidate imageConversationHistoryItem) imageConversationHistoryItem {
	var currentMap, candidateMap map[string]any
	if json.Unmarshal(current.raw, &currentMap) != nil || json.Unmarshal(candidate.raw, &candidateMap) != nil {
		if historyTimeAtOrAfter(candidate.updatedAt, current.updatedAt) {
			return candidate
		}
		return current
	}
	preferred := currentMap
	updatedAt := current.updatedAt
	candidateNewer := historyTimeAfter(candidate.updatedAt, current.updatedAt)
	conversationTimesTie := !candidateNewer && !historyTimeAfter(current.updatedAt, candidate.updatedAt)
	preferCandidate := candidateNewer || conversationTimesTie
	if preferCandidate {
		preferred = candidateMap
		updatedAt = candidate.updatedAt
	}
	preferred["turns"] = mergeImageConversationTurns(
		util.AsMapSlice(currentMap["turns"]),
		util.AsMapSlice(candidateMap["turns"]),
	)
	preferred["updatedAt"] = updatedAt
	stripHeavyImageConversationFields(preferred)
	encoded, err := json.Marshal(preferred)
	if err != nil {
		if preferCandidate {
			return candidate
		}
		return current
	}
	return imageConversationHistoryItem{id: current.id, updatedAt: updatedAt, raw: encoded}
}

func imageConversationTurnProgressScore(turn map[string]any) int {
	statusScore := 0
	switch util.Clean(turn["status"]) {
	case "success", "message":
		statusScore = 100
	case "error", "cancelled":
		statusScore = 90
	case "generating":
		statusScore = 50
	case "queued":
		statusScore = 20
	}
	settled, success := 0, 0
	for _, image := range util.AsMapSlice(turn["images"]) {
		status := util.Clean(image["status"])
		if status != "" && status != "loading" {
			settled++
		}
		if status == "success" || status == "message" {
			success++
		}
	}
	return statusScore*1000 + success*10 + settled
}

func imageConversationTurnUpdatedAt(turn map[string]any) string {
	return NormalizeHistoryTime(util.Clean(turn["updatedAt"]), util.Clean(turn["createdAt"]))
}

func mergeImageConversationTurns(left, right []map[string]any) []map[string]any {
	turns := make([]map[string]any, 0, len(left)+len(right))
	indexByID := make(map[string]int, len(left)+len(right))
	for _, turn := range append(append([]map[string]any(nil), left...), right...) {
		id := strings.TrimSpace(util.Clean(turn["id"]))
		if id == "" {
			turns = append(turns, turn)
			continue
		}
		if index, ok := indexByID[id]; ok {
			current := turns[index]
			currentUpdatedAt := imageConversationTurnUpdatedAt(current)
			candidateUpdatedAt := imageConversationTurnUpdatedAt(turn)
			currentScore := imageConversationTurnProgressScore(current)
			candidateScore := imageConversationTurnProgressScore(turn)
			candidateNewer := historyTimeAfter(candidateUpdatedAt, currentUpdatedAt)
			timesTie := !candidateNewer && !historyTimeAfter(currentUpdatedAt, candidateUpdatedAt)
			if candidateNewer || (timesTie && candidateScore >= currentScore) {
				turns[index] = turn
			}
			continue
		}
		indexByID[id] = len(turns)
		turns = append(turns, turn)
	}
	sort.SliceStable(turns, func(i, j int) bool {
		return historyTimeAfter(util.Clean(turns[j]["createdAt"]), util.Clean(turns[i]["createdAt"]))
	})
	return turns
}

func resolveImageConversationHistoryDocument(items []imageConversationHistoryItem, deletions []HistoryDeletion) ImageConversationHistoryDocument {
	deleteMap := HistoryDeletionMap(deletions)
	result := ImageConversationHistoryDocument{Items: []json.RawMessage{}, Deletions: []HistoryDeletion{}}
	for _, item := range items {
		if deletion, ok := deleteMap[item.id]; ok && TombstoneBlocksItem(deletion, item.updatedAt) {
			continue
		}
		if deletion, ok := deleteMap[item.id]; ok && !TombstoneBlocksItem(deletion, item.updatedAt) {
			delete(deleteMap, item.id)
		}
		result.Items = append(result.Items, append(json.RawMessage(nil), item.raw...))
	}
	for _, deletion := range deleteMap {
		result.Deletions = append(result.Deletions, deletion)
	}
	sort.SliceStable(result.Items, func(i, j int) bool {
		return historyTimeAfter(imageConversationHistoryUpdatedAt(result.Items[i]), imageConversationHistoryUpdatedAt(result.Items[j]))
	})
	result.Deletions = SortHistoryDeletions(result.Deletions, maxImageConversationHistoryDeletes)
	if len(result.Items) > maxImageConversationHistoryItems {
		result.Items = result.Items[:maxImageConversationHistoryItems]
	}
	return result
}

func imageConversationHistoryUpdatedAt(raw json.RawMessage) string {
	var item map[string]any
	if json.Unmarshal(raw, &item) != nil {
		return ""
	}
	return util.Clean(item["updatedAt"])
}

func cloneImageConversationHistoryDocument(document ImageConversationHistoryDocument) ImageConversationHistoryDocument {
	result := ImageConversationHistoryDocument{
		Items:     make([]json.RawMessage, len(document.Items)),
		Deletions: append([]HistoryDeletion(nil), document.Deletions...),
	}
	for index, item := range document.Items {
		result.Items[index] = append(json.RawMessage(nil), item...)
	}
	return result
}

// stripHeavyImageConversationFields removes multi-megabyte base64 payloads from
// conversation history while preserving durable url/path references.
// This keeps GET/PUT /api/image-conversations fast as histories grow.
//
// Uploaded references that only exist as dataUrl are kept so regenerate/edit still works.
// Once a durable url/path is present, inline payloads are dropped.
func stripHeavyImageConversationFields(value any) {
	switch node := value.(type) {
	case map[string]any:
		url := strings.TrimSpace(util.Clean(node["url"]))
		path := strings.TrimSpace(util.Clean(node["path"]))
		hasDurableRef := url != "" || path != ""

		if hasDurableRef {
			delete(node, "dataUrl")
			delete(node, "data_url")
			delete(node, "b64_json")
		}
		for _, child := range node {
			stripHeavyImageConversationFields(child)
		}
	case []any:
		for _, child := range node {
			stripHeavyImageConversationFields(child)
		}
	}
}
