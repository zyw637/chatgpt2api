package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExternalChatMigratesLegacyImageProvider(t *testing.T) {
	backend := newTestStorageBackend(t)
	store := jsonDocumentStoreFromBackend(backend)
	legacy := map[string]any{
		"items": []map[string]any{{
			"id": "legacy-provider", "name": "Legacy", "enabled": true,
			"protocol": ExternalImageProtocolImages, "base_url": "https://example.com/v1", "api_key": "legacy-key",
			"models": []string{"gpt-image-2"}, "default_model": "gpt-image-2", "timeout_seconds": 30,
		}},
		"settings": map[string]any{},
	}
	if err := saveStoredJSON(store, externalImageProvidersDocument, legacy); err != nil {
		t.Fatal(err)
	}
	svc := newTestExternalImageService(t, backend, testExternalImageConfig{root: t.TempDir()})
	items := svc.ListProviders(false)
	if len(items) != 1 || items[0]["image_enabled"] != true || items[0]["chat_enabled"] != false {
		t.Fatalf("migrated providers = %#v", items)
	}
	raw := loadStoredJSON(store, externalImageProvidersDocument)
	data, _ := json.Marshal(raw)
	if !strings.Contains(string(data), `"image_enabled":true`) {
		t.Fatalf("migrated provider was not persisted: %s", data)
	}
	adminData, _ := json.Marshal(svc.ListProviders(true))
	if strings.Contains(string(adminData), `"chat_models":null`) || !strings.Contains(string(adminData), `"chat_models":[]`) {
		t.Fatalf("empty chat models were not serialized as an array: %s", adminData)
	}
}

func TestExternalChatMigrationDoesNotOverwriteInvalidLegacyProviders(t *testing.T) {
	backend := newTestStorageBackend(t)
	store := jsonDocumentStoreFromBackend(backend)
	legacy := map[string]any{
		"items": []map[string]any{
			{
				"id": "valid-provider", "name": "Valid", "enabled": true,
				"protocol": ExternalImageProtocolImages, "base_url": "https://example.com/v1", "api_key": "valid-key",
				"models": []string{"gpt-image-2"}, "default_model": "gpt-image-2", "timeout_seconds": 30,
			},
			{
				"id": "invalid-provider", "name": "Invalid", "enabled": true,
				"protocol": ExternalImageProtocolImages, "base_url": "http://example.com/v1", "api_key": "invalid-key",
				"models": []string{"gpt-image-2"}, "default_model": "gpt-image-2", "timeout_seconds": 30,
			},
		},
		"settings": map[string]any{},
	}
	if err := saveStoredJSON(store, externalImageProvidersDocument, legacy); err != nil {
		t.Fatal(err)
	}
	svc := newTestExternalImageService(t, backend, testExternalImageConfig{root: t.TempDir()})
	if len(svc.ListProviders(true)) != 1 {
		t.Fatalf("normalized provider count = %d, want 1", len(svc.ListProviders(true)))
	}
	raw := loadStoredJSON(store, externalImageProvidersDocument)
	data, _ := json.Marshal(raw)
	var stored struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Items) != 2 {
		t.Fatalf("stored legacy providers = %d, want 2: %s", len(stored.Items), data)
	}
	if _, migrated := stored.Items[0]["image_enabled"]; migrated {
		t.Fatalf("partially migrated document was persisted: %s", data)
	}
}

func TestExternalChatStreamsOpenAICompatibleResponse(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer chat-secret" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "req-chat-1")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	svc := newTestExternalImageService(t, newTestStorageBackend(t), testExternalImageConfig{root: t.TempDir()})
	provider, err := svc.CreateProvider(ExternalImageProvider{
		Name: "Chat", Enabled: true, ChatEnabled: true, ChatProtocol: ExternalChatProtocolOpenAI,
		BaseURL: server.URL + "/v1", APIKey: "chat-secret", ChatModels: []string{"chat-model"}, ChatDefaultModel: "chat-model", TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.ListProviders(false)) != 0 || len(svc.ListChatProviders()) != 1 {
		t.Fatalf("chat-only provider leaked into image providers")
	}
	stream, err := svc.StartChat(context.Background(), testExternalIdentity("chat-owner"), ExternalChatSubmission{
		ProviderID: provider["id"].(string), Messages: []ExternalChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(stream.Body)
	stream.Body.Close()
	stream.Release()
	svc.mu.Lock()
	running := svc.chatRunning["chat-owner"]
	svc.mu.Unlock()
	if running != 0 {
		t.Fatalf("chat running count = %d after stream release", running)
	}
	if err != nil || !strings.Contains(string(data), "hello") {
		t.Fatalf("stream body = %q, error = %v", data, err)
	}
	if received["stream"] != true || received["model"] != "chat-model" {
		t.Fatalf("upstream payload = %#v", received)
	}
	if stream.RequestID != "req-chat-1" {
		t.Fatalf("request id = %q", stream.RequestID)
	}
}

func TestExternalChatCapabilityAndLimitValidation(t *testing.T) {
	svc := newTestExternalImageService(t, newTestStorageBackend(t), testExternalImageConfig{root: t.TempDir()})
	if _, err := svc.CreateProvider(ExternalImageProvider{Name: "Empty", Enabled: true, BaseURL: "https://example.com/v1", APIKey: "key"}); err == nil {
		t.Fatal("empty capability provider was accepted")
	}
	if _, err := svc.CreateProvider(ExternalImageProvider{Name: "Chat", Enabled: true, ChatEnabled: true, BaseURL: "https://example.com/v1", APIKey: "key", ChatModels: []string{"chat-model"}, ChatDefaultModel: "chat-model", ChatConcurrencyLimit: 1, TimeoutSeconds: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSettings(ExternalImageSettings{UserConcurrentLimit: 2, UserRPMLimit: 10, ChatUserConcurrentLimit: 1, ChatUserRPMLimit: 1}); err != nil {
		t.Fatal(err)
	}
	if err := normalizeExternalChatSubmission(&ExternalChatSubmission{ProviderID: "id", Messages: []ExternalChatMessage{{Role: "tool", Content: "x"}}}); err == nil {
		t.Fatal("unsupported role was accepted")
	}
	if err := normalizeExternalChatSubmission(&ExternalChatSubmission{ProviderID: "id", Messages: []ExternalChatMessage{{Role: "user", Content: "x"}}, MaxCompletionTokens: func() *int { value := 0; return &value }()}); err == nil {
		t.Fatal("invalid max completion tokens were accepted")
	}
}

func TestExternalChatEnforcesConcurrentAndRPMLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	svc := newTestExternalImageService(t, newTestStorageBackend(t), testExternalImageConfig{root: t.TempDir()})
	provider, err := svc.CreateProvider(ExternalImageProvider{
		Name: "Limited Chat", Enabled: true, ChatEnabled: true,
		BaseURL: server.URL + "/v1", APIKey: "key", ChatModels: []string{"chat-model"}, ChatDefaultModel: "chat-model", ChatConcurrencyLimit: 1, TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSettings(ExternalImageSettings{UserConcurrentLimit: 2, UserRPMLimit: 10, ChatUserConcurrentLimit: 1, ChatUserRPMLimit: 1}); err != nil {
		t.Fatal(err)
	}
	identity := testExternalIdentity("limited-owner")
	submission := ExternalChatSubmission{ProviderID: provider["id"].(string), Messages: []ExternalChatMessage{{Role: "user", Content: "hello"}}}
	stream, err := svc.StartChat(context.Background(), identity, submission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartChat(context.Background(), identity, submission); err == nil || !strings.Contains(err.Error(), "concurrent") {
		t.Fatalf("concurrent request error = %v", err)
	}
	_, _ = io.Copy(io.Discard, stream.Body)
	_ = stream.Body.Close()
	stream.Release()
	if _, err := svc.StartChat(context.Background(), identity, submission); err == nil || !strings.Contains(err.Error(), "RPM") {
		t.Fatalf("RPM request error = %v", err)
	}
}

func TestExternalChatClassifiesTransportErrors(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	baseURL := server.URL
	server.Close()
	svc := newTestExternalImageService(t, newTestStorageBackend(t), testExternalImageConfig{root: t.TempDir()})
	provider, err := svc.CreateProvider(ExternalImageProvider{
		Name: "Offline Chat", Enabled: true, ChatEnabled: true,
		BaseURL: baseURL + "/v1", APIKey: "key", ChatModels: []string{"chat-model"}, ChatDefaultModel: "chat-model", TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.StartChat(context.Background(), testExternalIdentity("offline-owner"), ExternalChatSubmission{
		ProviderID: provider["id"].(string), Messages: []ExternalChatMessage{{Role: "user", Content: "hello"}},
	})
	var transportErr ExternalChatTransportError
	if !errors.As(err, &transportErr) {
		t.Fatalf("StartChat() error = %v, want transport error", err)
	}
}

func TestNormalizeExternalChatSubmissionDropsEmptyAssistantPlaceholder(t *testing.T) {
	submission := ExternalChatSubmission{
		ProviderID: "provider-1",
		Messages: []ExternalChatMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "  "},
			{Role: "user", Content: "continue"},
		},
	}
	if err := normalizeExternalChatSubmission(&submission); err != nil {
		t.Fatalf("normalizeExternalChatSubmission() error = %v", err)
	}
	if len(submission.Messages) != 2 || submission.Messages[0].Content != "hello" || submission.Messages[1].Content != "continue" {
		t.Fatalf("normalized messages = %#v", submission.Messages)
	}

	submission.Messages = []ExternalChatMessage{{Role: "user", Content: ""}}
	if err := normalizeExternalChatSubmission(&submission); err == nil || err.Error() != "message content is required" {
		t.Fatalf("empty user message error = %v", err)
	}
}
