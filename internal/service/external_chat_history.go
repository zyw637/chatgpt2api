package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"chatgpt2api/internal/storage"
	"chatgpt2api/internal/util"
)

const (
	externalChatHistoryDocumentDir = "external_chat_conversations"
	maxExternalChatConversations   = 50
	maxExternalChatMessages        = 200
	maxExternalChatDeletions       = 500
	maxExternalChatMessageDeletes  = 500
)

type ExternalChatHistoryMessage struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"createdAt"`
	Status    string `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

type ExternalChatHistoryConversation struct {
	ID                  string                       `json:"id"`
	Title               string                       `json:"title"`
	ProviderID          string                       `json:"providerId"`
	Model               string                       `json:"model"`
	SystemPrompt        string                       `json:"systemPrompt"`
	Temperature         string                       `json:"temperature"`
	MaxCompletionTokens string                       `json:"maxCompletionTokens"`
	CreatedAt           string                       `json:"createdAt"`
	UpdatedAt           string                       `json:"updatedAt"`
	Messages            []ExternalChatHistoryMessage `json:"messages"`
	MessageDeletions    []HistoryDeletion            `json:"messageDeletions,omitempty"`
}

// ExternalChatHistoryDeletion is kept as an alias for JSON/API stability.
type ExternalChatHistoryDeletion = HistoryDeletion

type ExternalChatHistoryDocument struct {
	Items     []ExternalChatHistoryConversation `json:"items"`
	Deletions []HistoryDeletion                 `json:"deletions"`
}

type ExternalChatHistoryInputError struct {
	Message string
}

func (e ExternalChatHistoryInputError) Error() string {
	return e.Message
}

type ExternalChatHistoryService struct {
	mu    sync.Mutex
	store storage.JSONDocumentBackend
}

func NewExternalChatHistoryService(backend ...storage.Backend) *ExternalChatHistoryService {
	return &ExternalChatHistoryService{store: firstJSONDocumentStore(backend)}
}

func (s *ExternalChatHistoryService) List(ownerID string) (ExternalChatHistoryDocument, error) {
	ownerID = util.Clean(ownerID)
	if ownerID == "" {
		return ExternalChatHistoryDocument{}, ExternalChatHistoryInputError{Message: "owner_id is required"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(ownerID)
}

func (s *ExternalChatHistoryService) Sync(ownerID string, incoming ExternalChatHistoryDocument) (ExternalChatHistoryDocument, error) {
	ownerID = util.Clean(ownerID)
	if ownerID == "" {
		return ExternalChatHistoryDocument{}, ExternalChatHistoryInputError{Message: "owner_id is required"}
	}
	normalized, err := normalizeExternalChatHistoryDocument(incoming)
	if err != nil {
		return ExternalChatHistoryDocument{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.loadLocked(ownerID)
	if err != nil {
		return ExternalChatHistoryDocument{}, err
	}
	merged := mergeExternalChatHistory(stored, normalized)
	if err := saveStoredJSON(s.store, externalChatHistoryDocumentName(ownerID), merged); err != nil {
		return ExternalChatHistoryDocument{}, err
	}
	return cloneExternalChatHistoryDocument(merged), nil
}

func (s *ExternalChatHistoryService) loadLocked(ownerID string) (ExternalChatHistoryDocument, error) {
	if s.store == nil {
		return ExternalChatHistoryDocument{}, fmt.Errorf("storage document backend is required")
	}
	raw, err := s.store.LoadJSONDocument(externalChatHistoryDocumentName(ownerID))
	if err != nil {
		return ExternalChatHistoryDocument{}, err
	}
	if raw == nil {
		return ExternalChatHistoryDocument{Items: []ExternalChatHistoryConversation{}, Deletions: []HistoryDeletion{}}, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ExternalChatHistoryDocument{}, fmt.Errorf("encode external chat history: %w", err)
	}
	var document ExternalChatHistoryDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		return ExternalChatHistoryDocument{}, fmt.Errorf("decode external chat history: %w", err)
	}
	return normalizeExternalChatHistoryDocument(document)
}

func externalChatHistoryDocumentName(ownerID string) string {
	return externalChatHistoryDocumentDir + "/" + util.SHA256Hex(ownerID) + ".json"
}

func normalizeExternalChatHistory(items []ExternalChatHistoryConversation) ([]ExternalChatHistoryConversation, error) {
	now := util.NowISO()
	normalized := make([]ExternalChatHistoryConversation, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for index, item := range items {
		conversation, err := normalizeExternalChatHistoryConversation(item, now)
		if err != nil {
			return nil, ExternalChatHistoryInputError{Message: fmt.Sprintf("conversation %d: %s", index+1, err)}
		}
		if _, ok := seen[conversation.ID]; ok {
			return nil, ExternalChatHistoryInputError{Message: fmt.Sprintf("conversation %d: duplicate id", index+1)}
		}
		seen[conversation.ID] = struct{}{}
		normalized = append(normalized, conversation)
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		return historyTimeAfter(normalized[i].UpdatedAt, normalized[j].UpdatedAt)
	})
	if len(normalized) > maxExternalChatConversations {
		normalized = normalized[:maxExternalChatConversations]
	}
	return cloneExternalChatHistory(normalized), nil
}

func normalizeExternalChatHistoryDocument(document ExternalChatHistoryDocument) (ExternalChatHistoryDocument, error) {
	items, err := normalizeExternalChatHistory(document.Items)
	if err != nil {
		return ExternalChatHistoryDocument{}, err
	}
	deletions, err := NormalizeHistoryDeletions(document.Deletions, maxExternalChatDeletions)
	if err != nil {
		return ExternalChatHistoryDocument{}, ExternalChatHistoryInputError{Message: err.Error()}
	}
	return resolveExternalChatHistoryDocument(ExternalChatHistoryDocument{Items: items, Deletions: deletions}), nil
}

func mergeExternalChatHistory(left, right ExternalChatHistoryDocument) ExternalChatHistoryDocument {
	items := make(map[string]ExternalChatHistoryConversation, len(left.Items)+len(right.Items))
	for _, item := range append(append([]ExternalChatHistoryConversation(nil), left.Items...), right.Items...) {
		current, ok := items[item.ID]
		if !ok {
			items[item.ID] = item
			continue
		}
		items[item.ID] = mergeExternalChatConversation(current, item)
	}
	deletionMap := HistoryDeletionMap(MergeHistoryDeletions(left.Deletions, right.Deletions))
	document := ExternalChatHistoryDocument{
		Items:     make([]ExternalChatHistoryConversation, 0, len(items)),
		Deletions: make([]HistoryDeletion, 0, len(deletionMap)),
	}
	for id, item := range items {
		if deletion, ok := deletionMap[id]; ok {
			if TombstoneBlocksItem(deletion, item.UpdatedAt) {
				continue
			}
			delete(deletionMap, id)
		}
		document.Items = append(document.Items, item)
	}
	for _, item := range deletionMap {
		document.Deletions = append(document.Deletions, item)
	}
	return resolveExternalChatHistoryDocument(document)
}

func mergeExternalChatConversation(current, candidate ExternalChatHistoryConversation) ExternalChatHistoryConversation {
	preferred := current
	if historyTimeAtOrAfter(candidate.UpdatedAt, current.UpdatedAt) {
		preferred = candidate
	}
	deletions := SortHistoryDeletions(
		MergeHistoryDeletions(current.MessageDeletions, candidate.MessageDeletions),
		maxExternalChatMessageDeletes,
	)
	preferred.Messages = mergeExternalChatMessages(current.Messages, candidate.Messages, deletions)
	preferred.MessageDeletions = deletions
	return preferred
}

func externalChatMessageStatusScore(status string) int {
	switch status {
	case "complete":
		return 4
	case "error":
		return 3
	case "stopped":
		return 2
	case "streaming":
		return 1
	default:
		return 0
	}
}

func mergeExternalChatMessages(left, right []ExternalChatHistoryMessage, deletions []HistoryDeletion) []ExternalChatHistoryMessage {
	messages := make([]ExternalChatHistoryMessage, 0, len(left)+len(right))
	indexByID := make(map[string]int, len(left)+len(right))
	for _, message := range append(append([]ExternalChatHistoryMessage(nil), left...), right...) {
		if index, ok := indexByID[message.ID]; ok {
			current := messages[index]
			currentScore := externalChatMessageStatusScore(current.Status)
			candidateScore := externalChatMessageStatusScore(message.Status)
			if candidateScore > currentScore || (candidateScore == currentScore && len(message.Content) >= len(current.Content)) {
				messages[index] = message
			}
			continue
		}
		indexByID[message.ID] = len(messages)
		messages = append(messages, message)
	}
	deleted := HistoryDeletionMap(deletions)
	filtered := messages[:0]
	for _, message := range messages {
		if _, ok := deleted[message.ID]; ok {
			continue
		}
		filtered = append(filtered, message)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return historyTimeAfter(filtered[j].CreatedAt, filtered[i].CreatedAt) })
	if len(filtered) > maxExternalChatMessages {
		filtered = filtered[len(filtered)-maxExternalChatMessages:]
	}
	if len(filtered) > 1 && filtered[0].Role == "assistant" {
		filtered = filtered[1:]
	}
	return append([]ExternalChatHistoryMessage(nil), filtered...)
}

func resolveExternalChatHistoryDocument(document ExternalChatHistoryDocument) ExternalChatHistoryDocument {
	deletions := HistoryDeletionMap(document.Deletions)
	items := document.Items[:0]
	for _, item := range document.Items {
		deletion, deleted := deletions[item.ID]
		if deleted && TombstoneBlocksItem(deletion, item.UpdatedAt) {
			continue
		}
		if deleted {
			delete(deletions, item.ID)
		}
		items = append(items, item)
	}
	document.Items = items
	document.Deletions = document.Deletions[:0]
	for _, item := range deletions {
		document.Deletions = append(document.Deletions, item)
	}
	sort.SliceStable(document.Items, func(i, j int) bool {
		return historyTimeAfter(document.Items[i].UpdatedAt, document.Items[j].UpdatedAt)
	})
	document.Deletions = SortHistoryDeletions(document.Deletions, maxExternalChatDeletions)
	if len(document.Items) > maxExternalChatConversations {
		document.Items = document.Items[:maxExternalChatConversations]
	}
	return document
}

func normalizeExternalChatHistoryConversation(item ExternalChatHistoryConversation, now string) (ExternalChatHistoryConversation, error) {
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		return ExternalChatHistoryConversation{}, fmt.Errorf("id is required")
	}
	item.Title = strings.TrimSpace(item.Title)
	if item.Title == "" {
		item.Title = "新对话"
	}
	item.ProviderID = strings.TrimSpace(item.ProviderID)
	item.Model = strings.TrimSpace(item.Model)
	item.Temperature = strings.TrimSpace(item.Temperature)
	if item.Temperature != "" {
		value, err := strconv.ParseFloat(item.Temperature, 64)
		if err != nil || value < 0 || value > 2 {
			return ExternalChatHistoryConversation{}, fmt.Errorf("temperature must be between 0 and 2")
		}
	}
	item.MaxCompletionTokens = strings.TrimSpace(item.MaxCompletionTokens)
	if item.MaxCompletionTokens != "" {
		value, err := strconv.Atoi(item.MaxCompletionTokens)
		if err != nil || value < 1 || value > 131072 {
			return ExternalChatHistoryConversation{}, fmt.Errorf("maxCompletionTokens must be between 1 and 131072")
		}
	}
	var err error
	item.CreatedAt, err = normalizeHistoryInputTime(item.CreatedAt, now)
	if err != nil {
		return ExternalChatHistoryConversation{}, fmt.Errorf("createdAt: %w", err)
	}
	item.UpdatedAt, err = normalizeHistoryInputTime(item.UpdatedAt, item.CreatedAt)
	if err != nil {
		return ExternalChatHistoryConversation{}, fmt.Errorf("updatedAt: %w", err)
	}
	messageDeletions, err := NormalizeHistoryDeletions(item.MessageDeletions, maxExternalChatMessageDeletes)
	if err != nil {
		return ExternalChatHistoryConversation{}, fmt.Errorf("message deletions: %w", err)
	}
	item.MessageDeletions = messageDeletions

	start := 0
	if len(item.Messages) > maxExternalChatMessages {
		start = len(item.Messages) - maxExternalChatMessages
	}
	messages := make([]ExternalChatHistoryMessage, 0, len(item.Messages)-start)
	seenMessages := make(map[string]struct{}, len(item.Messages)-start)
	for index, message := range item.Messages[start:] {
		normalized, err := normalizeExternalChatHistoryMessage(message, now)
		if err != nil {
			return ExternalChatHistoryConversation{}, fmt.Errorf("message %d: %s", start+index+1, err)
		}
		if _, ok := seenMessages[normalized.ID]; ok {
			return ExternalChatHistoryConversation{}, fmt.Errorf("message %d: duplicate id", start+index+1)
		}
		seenMessages[normalized.ID] = struct{}{}
		messages = append(messages, normalized)
	}
	if len(messages) > 1 && messages[0].Role == "assistant" {
		messages = messages[1:]
	}
	item.Messages = mergeExternalChatMessages(messages, nil, messageDeletions)
	return item, nil
}

func normalizeExternalChatHistoryMessage(item ExternalChatHistoryMessage, now string) (ExternalChatHistoryMessage, error) {
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		return ExternalChatHistoryMessage{}, fmt.Errorf("id is required")
	}
	if item.Role != "user" && item.Role != "assistant" {
		return ExternalChatHistoryMessage{}, fmt.Errorf("role must be user or assistant")
	}
	var err error
	item.CreatedAt, err = normalizeHistoryInputTime(item.CreatedAt, now)
	if err != nil {
		return ExternalChatHistoryMessage{}, fmt.Errorf("createdAt: %w", err)
	}
	switch item.Status {
	case "":
		if item.Error != "" {
			item.Status = "error"
		} else {
			item.Status = "complete"
		}
	case "streaming", "complete", "error", "stopped":
	default:
		return ExternalChatHistoryMessage{}, fmt.Errorf("invalid status")
	}
	return item, nil
}

// normalizeExternalChatHistoryTime kept for any remaining call sites; delegates to shared helper.
func normalizeExternalChatHistoryTime(value, fallback string) string {
	return NormalizeHistoryTime(value, fallback)
}

func cloneExternalChatHistory(items []ExternalChatHistoryConversation) []ExternalChatHistoryConversation {
	out := append([]ExternalChatHistoryConversation(nil), items...)
	for index := range out {
		out[index].Messages = append([]ExternalChatHistoryMessage(nil), out[index].Messages...)
		out[index].MessageDeletions = append([]HistoryDeletion(nil), out[index].MessageDeletions...)
	}
	return out
}

func cloneExternalChatHistoryDocument(document ExternalChatHistoryDocument) ExternalChatHistoryDocument {
	return ExternalChatHistoryDocument{
		Items:     cloneExternalChatHistory(document.Items),
		Deletions: append([]HistoryDeletion(nil), document.Deletions...),
	}
}
