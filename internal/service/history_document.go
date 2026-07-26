package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"chatgpt2api/internal/util"
)

const maxHistoryFutureSkew = 5 * time.Minute

// HistoryDeletion is a tombstone for a conversation/history item.
// When deletedAt >= item.updatedAt the item must not reappear after merge.
type HistoryDeletion struct {
	ID        string `json:"id"`
	DeletedAt string `json:"deletedAt"`
}

// NormalizeHistoryTime parses RFC3339/RFC3339Nano timestamps and falls back safely.
func NormalizeHistoryTime(value, fallback string) string {
	normalized, err := normalizeHistoryInputTime(value, fallback)
	if err == nil {
		return normalized
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func normalizeHistoryInputTime(value, fallback string) (string, error) {
	parsed, err := parseHistoryTime(value)
	if err != nil {
		parsed, err = parseHistoryTime(fallback)
	}
	if err != nil {
		parsed = time.Now().UTC()
	}
	if parsed.After(time.Now().UTC().Add(maxHistoryFutureSkew)) {
		return "", fmt.Errorf("timestamp is too far in the future")
	}
	return parsed.UTC().Format(time.RFC3339Nano), nil
}

func historyTimeAfter(left, right string) bool {
	leftTime, leftErr := parseHistoryTime(left)
	rightTime, rightErr := parseHistoryTime(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return leftTime.After(rightTime)
}

func historyTimeAtOrAfter(left, right string) bool {
	leftTime, leftErr := parseHistoryTime(left)
	rightTime, rightErr := parseHistoryTime(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return !leftTime.Before(rightTime)
}

func parseHistoryTime(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339, trimmed)
}

// NormalizeHistoryDeletions validates, dedupes (LWW by deletedAt), sorts, and caps deletions.
func NormalizeHistoryDeletions(items []HistoryDeletion, max int) ([]HistoryDeletion, error) {
	if max <= 0 {
		max = 500
	}
	now := util.NowISO()
	normalized := make([]HistoryDeletion, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for index, item := range items {
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" {
			return nil, fmt.Errorf("deletion %d: id is required", index+1)
		}
		if _, ok := seen[item.ID]; ok {
			return nil, fmt.Errorf("deletion %d: duplicate id", index+1)
		}
		seen[item.ID] = struct{}{}
		deletedAt, err := normalizeHistoryInputTime(item.DeletedAt, now)
		if err != nil {
			return nil, fmt.Errorf("deletion %d: %w", index+1, err)
		}
		item.DeletedAt = deletedAt
		normalized = append(normalized, item)
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		return historyTimeAfter(normalized[i].DeletedAt, normalized[j].DeletedAt)
	})
	if len(normalized) > max {
		normalized = normalized[:max]
	}
	return normalized, nil
}

// MergeHistoryDeletions merges deletion tombstones with LWW on deletedAt.
func MergeHistoryDeletions(left, right []HistoryDeletion) []HistoryDeletion {
	merged := make(map[string]HistoryDeletion, len(left)+len(right))
	for _, item := range append(append([]HistoryDeletion(nil), left...), right...) {
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" {
			continue
		}
		item.DeletedAt = NormalizeHistoryTime(item.DeletedAt, util.NowISO())
		current, ok := merged[item.ID]
		if !ok || historyTimeAtOrAfter(item.DeletedAt, current.DeletedAt) {
			merged[item.ID] = item
		}
	}
	out := make([]HistoryDeletion, 0, len(merged))
	for _, item := range merged {
		out = append(out, item)
	}
	return out
}

// HistoryDeletionMap builds an id→deletion map (LWW by deletedAt).
func HistoryDeletionMap(items []HistoryDeletion) map[string]HistoryDeletion {
	out := make(map[string]HistoryDeletion, len(items))
	for _, item := range items {
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" {
			continue
		}
		current, ok := out[item.ID]
		if !ok || historyTimeAtOrAfter(item.DeletedAt, current.DeletedAt) {
			out[item.ID] = item
		}
	}
	return out
}

// TombstoneBlocksItem reports whether a deletion tombstone should hide the item.
func TombstoneBlocksItem(deletion HistoryDeletion, updatedAt string) bool {
	return historyTimeAtOrAfter(deletion.DeletedAt, updatedAt)
}

// SortHistoryDeletions sorts deletions by deletedAt descending and caps length.
func SortHistoryDeletions(items []HistoryDeletion, max int) []HistoryDeletion {
	if max <= 0 {
		max = 500
	}
	sort.SliceStable(items, func(i, j int) bool {
		return historyTimeAfter(items[i].DeletedAt, items[j].DeletedAt)
	})
	if len(items) > max {
		items = items[:max]
	}
	return items
}
