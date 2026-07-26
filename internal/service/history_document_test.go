package service

import (
	"testing"
	"time"
)

func TestNormalizeHistoryDeletionsAndMerge(t *testing.T) {
	items, err := NormalizeHistoryDeletions([]HistoryDeletion{
		{ID: "a", DeletedAt: "2026-07-20T10:00:00Z"},
		{ID: "b", DeletedAt: "2026-07-20T11:00:00Z"},
	}, 500)
	if err != nil {
		t.Fatalf("NormalizeHistoryDeletions() error = %v", err)
	}
	if len(items) != 2 || items[0].ID != "b" {
		t.Fatalf("sorted deletions = %#v", items)
	}

	merged := MergeHistoryDeletions(items, []HistoryDeletion{
		{ID: "a", DeletedAt: "2026-07-20T12:00:00Z"},
	})
	m := HistoryDeletionMap(merged)
	if m["a"].DeletedAt < "2026-07-20T12:00:00" {
		t.Fatalf("LWW deletion lost: %#v", m["a"])
	}
	if !TombstoneBlocksItem(m["a"], "2026-07-20T11:00:00Z") {
		t.Fatal("expected tombstone to block older item")
	}
	if TombstoneBlocksItem(m["a"], "2026-07-20T13:00:00Z") {
		t.Fatal("newer item should revive past tombstone")
	}
}

func TestNormalizeHistoryDeletionsRejectsInvalid(t *testing.T) {
	if _, err := NormalizeHistoryDeletions([]HistoryDeletion{{ID: ""}}, 10); err == nil {
		t.Fatal("expected missing id error")
	}
	if _, err := NormalizeHistoryDeletions([]HistoryDeletion{
		{ID: "a", DeletedAt: "2026-07-20T10:00:00Z"},
		{ID: "a", DeletedAt: "2026-07-20T11:00:00Z"},
	}, 10); err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestHistoryTimeComparisonHandlesFractionalSeconds(t *testing.T) {
	whole := HistoryDeletion{ID: "item", DeletedAt: "2026-07-20T10:00:00Z"}
	if TombstoneBlocksItem(whole, "2026-07-20T10:00:00.5Z") {
		t.Fatal("whole-second tombstone blocked a newer fractional-second item")
	}
	fractional := HistoryDeletion{ID: "item", DeletedAt: "2026-07-20T10:00:00.5Z"}
	if !TombstoneBlocksItem(fractional, "2026-07-20T10:00:00Z") {
		t.Fatal("fractional-second tombstone did not block an older whole-second item")
	}
}

func TestNormalizeHistoryDeletionsRejectsFutureTimestamp(t *testing.T) {
	_, err := NormalizeHistoryDeletions([]HistoryDeletion{{ID: "item", DeletedAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}}, 10)
	if err == nil {
		t.Fatal("future deletion timestamp was accepted")
	}
}
