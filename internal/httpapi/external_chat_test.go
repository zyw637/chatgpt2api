package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chatgpt2api/internal/service"
)

func TestExternalChatHTTPStreamingAndAuditRedaction(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	app := newTestApp(t)
	defer app.Close()
	provider, err := app.external.CreateProvider(service.ExternalImageProvider{
		Name: "Chat HTTP", Enabled: true, ChatEnabled: true, ChatProtocol: service.ExternalChatProtocolOpenAI,
		BaseURL: upstream.URL + "/v1", APIKey: "chat-http-secret", ChatModels: []string{"chat-model"}, ChatDefaultModel: "chat-model", TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, userKey, err := app.auth.CreateAPIKey(service.AuthRoleUser, "chat-user", service.AuthOwner{})
	if err != nil {
		t.Fatal(err)
	}
	handler := app.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/external-chat-providers", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous providers status = %d", res.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/external-chat-providers", nil)
	req.Header.Set("Authorization", "Bearer "+userKey)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "chat-http-secret") || !strings.Contains(res.Body.String(), provider["id"].(string)) {
		t.Fatalf("providers response = %d %s", res.Code, res.Body.String())
	}

	payload := map[string]any{
		"provider_id": provider["id"],
		"model":       "chat-model",
		"messages":    []map[string]any{{"role": "user", "content": "audit-secret-prompt"}},
	}
	data, _ := json.Marshal(payload)
	req = httptest.NewRequest(http.MethodPost, "/api/external-chat/completions", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+userKey)
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "hello") || res.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("chat response = %d headers=%v body=%s", res.Code, res.Header(), res.Body.String())
	}

	logs, _ := json.Marshal(app.logs.List("", "", 100))
	if strings.Contains(string(logs), "audit-secret-prompt") || strings.Contains(string(logs), "chat-http-secret") {
		t.Fatalf("audit logs exposed chat content or API key: %s", logs)
	}
	if !strings.Contains(string(logs), "[REDACTED]") {
		t.Fatalf("audit logs did not record redacted messages: %s", logs)
	}
}

func TestExternalChatInterruptedUpstreamReportsPartialStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		flusher.Flush()
		hijacker, _ := w.(http.Hijacker)
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer upstream.Close()

	app := newTestApp(t)
	defer app.Close()
	provider, err := app.external.CreateProvider(service.ExternalImageProvider{
		Name: "Interrupted Chat", Enabled: true, ChatEnabled: true, ChatProtocol: service.ExternalChatProtocolOpenAI,
		BaseURL: upstream.URL + "/v1", APIKey: "chat-secret", ChatModels: []string{"chat-model"}, ChatDefaultModel: "chat-model", TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, userKey, err := app.auth.CreateAPIKey(service.AuthRoleUser, "chat-user", service.AuthOwner{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(map[string]any{
		"provider_id": provider["id"],
		"model":       "chat-model",
		"messages":    []map[string]any{{"role": "user", "content": "hello"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/external-chat/completions", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+userKey)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "partial") || !strings.Contains(res.Body.String(), "上游流中断") {
		t.Fatalf("interrupted stream response = %d %s", res.Code, res.Body.String())
	}
	item := findLogBySummary(app.logs.List("", "", 100), "API 聊天流中断")
	if item == nil {
		t.Fatal("interrupted stream log is missing")
	}
	detail, _ := item["detail"].(map[string]any)
	if detail["phase"] != "upstream_read" || fmt.Sprint(detail["status"]) != fmt.Sprint(http.StatusBadGateway) {
		t.Fatalf("interrupted stream log detail = %#v", detail)
	}
}

func TestExternalChatTransportErrorStatus(t *testing.T) {
	res := httptest.NewRecorder()
	writeExternalChatError(res, service.ExternalChatTransportError{Err: errors.New("connection refused")})
	if res.Code != http.StatusBadGateway {
		t.Fatalf("transport error status = %d, want %d", res.Code, http.StatusBadGateway)
	}
}

func TestExternalChatRequestFailureIncludesReasonInLog(t *testing.T) {
	app := newTestApp(t)
	defer app.Close()
	_, userKey, err := app.auth.CreateAPIKey(service.AuthRoleUser, "chat-user", service.AuthOwner{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/external-chat/completions", strings.NewReader(`{"provider_id":"","model":"chat-model","messages":[{"role":"user","content":"secret prompt"}]}`))
	req.Header.Set("Authorization", "Bearer "+userKey)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("request failure status = %d body = %s", res.Code, res.Body.String())
	}
	item := findLogBySummary(app.logs.List("", "", 100), "API 聊天调用失败")
	if item == nil {
		t.Fatal("request failure log is missing")
	}
	detail, _ := item["detail"].(map[string]any)
	if !strings.Contains(fmt.Sprint(detail["error"]), "provider_id is required") || strings.Contains(fmt.Sprint(detail), "secret prompt") {
		t.Fatalf("request failure log detail = %#v", detail)
	}
}

func TestExternalChatHistoryHTTPIsPersonalAndRedactsAuditContent(t *testing.T) {
	app := newTestApp(t)
	defer app.Close()

	_, aliceToken, err := app.auth.RegisterPasswordUser("chat-alice", "Password123", "Alice")
	if err != nil {
		t.Fatalf("RegisterPasswordUser(alice) error = %v", err)
	}
	_, bobToken, err := app.auth.RegisterPasswordUser("chat-bob", "Password123", "Bob")
	if err != nil {
		t.Fatalf("RegisterPasswordUser(bob) error = %v", err)
	}

	payload := `{"items":[{"id":"chat-1","title":"private-title","providerId":"provider-1","model":"model-1","systemPrompt":"private-system-prompt","temperature":"0.7","maxCompletionTokens":"1024","createdAt":"2026-07-18T11:00:00Z","updatedAt":"2026-07-18T12:00:00Z","messages":[{"id":"message-1","role":"user","content":"private-message-content","createdAt":"2026-07-18T11:00:00Z","status":"complete"}]}]}`
	handler := app.Handler()

	req := httptest.NewRequest(http.MethodPut, "/api/external-chat-conversations", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("save history status = %d body = %s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/external-chat-conversations", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "private-message-content") {
		t.Fatalf("alice history status = %d body = %s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/external-chat-conversations", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "private-message-content") {
		t.Fatalf("bob history status = %d body = %s", res.Code, res.Body.String())
	}

	items := app.logs.List("", "", 100)
	encodedLogs, _ := json.Marshal(items)
	for _, secret := range []string{"private-title", "private-system-prompt", "private-message-content"} {
		if strings.Contains(string(encodedLogs), secret) {
			t.Fatalf("audit logs exposed %q: %s", secret, encodedLogs)
		}
	}
	auditLog := findLogByDetails(items, map[string]any{
		"method": http.MethodPut,
		"path":   "/api/external-chat-conversations",
	})
	if auditLog == nil {
		t.Fatalf("history update audit log missing: %#v", items)
	}
	detail, _ := auditLog["detail"].(map[string]any)
	requestArgs, _ := detail["request_args"].(map[string]any)
	if fmt.Sprint(requestArgs["conversation_count"]) != "1" || fmt.Sprint(requestArgs["message_count"]) != "1" || detail["response_body"] != nil {
		t.Fatalf("history audit detail = %#v", detail)
	}
}
