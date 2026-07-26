package service

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"chatgpt2api/internal/storage"
	"chatgpt2api/internal/util"
)

const (
	ImageConversationSessionActive = "active"
	ImageConversationSessionFailed = "failed"

	imageConversationSessionDocumentName = "image_conversation_sessions.json"
)

type ImageConversationSession struct {
	OwnerID                 string    `json:"owner_id"`
	FrontendConversationID  string    `json:"frontend_conversation_id"`
	AccessToken             string    `json:"access_token"`
	UpstreamConversationID  string    `json:"upstream_conversation_id"`
	UpstreamParentMessageID string    `json:"upstream_parent_message_id"`
	Status                  string    `json:"status"`
	CreatedAt               time.Time `json:"created_at"`
	LastUsedAt              time.Time `json:"last_used_at"`
}

type ImageConversationSessionService struct {
	mu      sync.RWMutex
	path    string
	store   storage.JSONDocumentBackend
	docName string
	items   map[string]ImageConversationSession
	initErr error
}

func NewImageConversationSessionService(path string, backends ...storage.Backend) *ImageConversationSessionService {
	s := &ImageConversationSessionService{
		path:    path,
		store:   firstJSONDocumentStore(backends),
		docName: imageConversationSessionDocumentName,
		items:   map[string]ImageConversationSession{},
	}
	s.items, s.initErr = s.load()
	return s
}

func (s *ImageConversationSessionService) InitializationError() error { return s.initErr }

func (s *ImageConversationSessionService) Get(ownerID, frontendConversationID string) (ImageConversationSession, bool) {
	if s == nil {
		return ImageConversationSession{}, false
	}
	key := imageConversationSessionKey(ownerID, frontendConversationID)
	if key == "" {
		return ImageConversationSession{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.items[key]
	return item, ok
}

func (s *ImageConversationSessionService) Bind(item ImageConversationSession) error {
	if s == nil {
		return fmt.Errorf("image conversation session service is nil")
	}
	item.OwnerID = util.Clean(item.OwnerID)
	item.FrontendConversationID = util.Clean(item.FrontendConversationID)
	item.AccessToken = strings.TrimSpace(item.AccessToken)
	item.UpstreamConversationID = util.Clean(item.UpstreamConversationID)
	item.UpstreamParentMessageID = util.Clean(item.UpstreamParentMessageID)
	key := imageConversationSessionKey(item.OwnerID, item.FrontendConversationID)
	if key == "" || item.AccessToken == "" || item.UpstreamConversationID == "" || item.UpstreamParentMessageID == "" {
		return fmt.Errorf("incomplete image conversation session binding")
	}

	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, hadExisting := s.items[key]
	if hadExisting && item.CreatedAt.IsZero() {
		item.CreatedAt = existing.CreatedAt
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	if item.LastUsedAt.IsZero() {
		item.LastUsedAt = now
	}
	item.Status = ImageConversationSessionActive
	if s.items == nil {
		s.items = map[string]ImageConversationSession{}
	}
	s.items[key] = item
	if s.store != nil {
		if err := s.saveLocked(); err != nil {
			if hadExisting {
				s.items[key] = existing
			} else {
				delete(s.items, key)
			}
			return fmt.Errorf("persist image conversation session binding: %w", err)
		}
	}
	return nil
}

func (s *ImageConversationSessionService) Invalidate(ownerID, frontendConversationID string) error {
	if s == nil {
		return fmt.Errorf("image conversation session service is nil")
	}
	key := imageConversationSessionKey(ownerID, frontendConversationID)
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[key]
	if !ok {
		return nil
	}
	previous := item
	item.Status = ImageConversationSessionFailed
	item.LastUsedAt = time.Now().UTC()
	s.items[key] = item
	if s.store != nil {
		if err := s.saveLocked(); err != nil {
			s.items[key] = previous
			return fmt.Errorf("persist image conversation session invalidation: %w", err)
		}
	}
	return nil
}

func (s *ImageConversationSessionService) Cleanup(maxAge time.Duration) (int, error) {
	if s == nil || maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := make(map[string]ImageConversationSession, len(s.items))
	for key, item := range s.items {
		previous[key] = item
	}
	removed := 0
	for key, item := range s.items {
		lastUsed := item.LastUsedAt
		if lastUsed.IsZero() {
			lastUsed = item.CreatedAt
		}
		if !lastUsed.IsZero() && lastUsed.Before(cutoff) {
			delete(s.items, key)
			removed++
		}
	}
	if removed > 0 && s.store != nil {
		if err := s.saveLocked(); err != nil {
			s.items = previous
			return 0, fmt.Errorf("persist image conversation session cleanup: %w", err)
		}
	}
	return removed, nil
}

func (s *ImageConversationSessionService) load() (map[string]ImageConversationSession, error) {
	if s.store == nil {
		return map[string]ImageConversationSession{}, nil
	}
	raw, err := s.store.LoadJSONDocument(s.docName)
	if err != nil {
		return nil, fmt.Errorf("load image conversation sessions: %w", err)
	}
	if obj, ok := raw.(map[string]any); ok {
		raw = obj["sessions"]
	}
	items := map[string]ImageConversationSession{}
	for _, rawItem := range util.AsMapSlice(raw) {
		item := ImageConversationSession{
			OwnerID:                 util.Clean(rawItem["owner_id"]),
			FrontendConversationID:  util.Clean(rawItem["frontend_conversation_id"]),
			AccessToken:             strings.TrimSpace(util.Clean(rawItem["access_token"])),
			UpstreamConversationID:  util.Clean(rawItem["upstream_conversation_id"]),
			UpstreamParentMessageID: util.Clean(rawItem["upstream_parent_message_id"]),
			Status:                  util.Clean(rawItem["status"]),
			CreatedAt:               parseImageConversationSessionTime(rawItem["created_at"]),
			LastUsedAt:              parseImageConversationSessionTime(rawItem["last_used_at"]),
		}
		key := imageConversationSessionKey(item.OwnerID, item.FrontendConversationID)
		if key == "" || item.AccessToken == "" || item.UpstreamConversationID == "" || item.UpstreamParentMessageID == "" {
			continue
		}
		if item.Status != ImageConversationSessionFailed {
			item.Status = ImageConversationSessionActive
		}
		items[key] = item
	}
	return items, nil
}

func (s *ImageConversationSessionService) saveLocked() error {
	items := make([]ImageConversationSession, 0, len(s.items))
	for _, item := range s.items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].LastUsedAt.After(items[j].LastUsedAt)
	})
	return saveStoredJSON(s.store, s.docName, map[string]any{"sessions": items})
}

func imageConversationSessionKey(ownerID, frontendConversationID string) string {
	ownerID = util.Clean(ownerID)
	frontendConversationID = util.Clean(frontendConversationID)
	if ownerID == "" || frontendConversationID == "" {
		return ""
	}
	return ownerID + "\x00" + frontendConversationID
}

func parseImageConversationSessionTime(value any) time.Time {
	if t, ok := value.(time.Time); ok {
		return t
	}
	text := util.Clean(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
