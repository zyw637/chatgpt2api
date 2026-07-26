package util

import (
	"strings"
	"testing"
)

func TestDecodeJSONRejectsTrailingValues(t *testing.T) {
	for _, input := range []string{`{"ok":true}{"extra":true}`, `{"ok":true} trailing`} {
		var value map[string]any
		if err := DecodeJSON(strings.NewReader(input), &value); err == nil {
			t.Fatalf("DecodeJSON(%q) accepted trailing data", input)
		}
	}
}

func TestDecodeJSONAllowsTrailingWhitespace(t *testing.T) {
	var value map[string]any
	if err := DecodeJSON(strings.NewReader("{\"ok\":true}\n  \t"), &value); err != nil {
		t.Fatalf("DecodeJSON() error = %v", err)
	}
}
