package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"chatgpt2api/internal/service"
	"chatgpt2api/internal/util"
)

const (
	maxAuditRequestPayloadBytes  = 64 * 1024
	maxAuditResponsePayloadBytes = 8 * 1024
	maxJSONRequestBodyBytes      = 8 << 20
	maxMultipartRequestBodyBytes = 128 << 20
	maxExternalMultipartBytes    = 50 << 20
)

type requestIdentityContextKey struct{}
type auditRequestContextKey struct{}
type businessLogContextKey struct{}
type requestStartedContextKey struct{}

type auditRequestCapture struct {
	args      any
	truncated bool
}

type auditBodyReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *auditBodyReadCloser) Close() error {
	if r == nil || r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len() < maxAuditResponsePayloadBytes {
		remaining := maxAuditResponsePayloadBytes - w.body.Len()
		if len(data) > remaining {
			_, _ = w.body.Write(data[:remaining])
		} else {
			_, _ = w.body.Write(data)
		}
	}
	return w.ResponseWriter.Write(data)
}

func (w *auditResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *auditResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (a *App) serveObservedHTTP(w http.ResponseWriter, r *http.Request, routes []appRoute) {
	if r.Method == http.MethodOptions || !isAPISpace(r.URL.Path) {
		a.serveHTTP(w, r, routes)
		return
	}

	recorder := &auditResponseWriter{ResponseWriter: w}
	start := time.Now()
	*r = *r.WithContext(context.WithValue(r.Context(), requestStartedContextKey{}, start))
	requestCapture := auditRequestCapture{}
	if applyRequestBodyLimit(recorder, r) {
		requestCapture = captureAuditRequest(r)
		*r = *r.WithContext(withAuditRequestCapture(r.Context(), requestCapture))
		a.serveHTTP(recorder, r, routes)
	}
	duration := time.Since(start)
	status := recorder.statusCode()

	a.logHTTPRequest(r, status, duration)
	if shouldWriteAuditLog(r, status) {
		a.writeAuditLog(r, recorder, status, duration, requestCapture)
	}
}

func applyRequestBodyLimit(w http.ResponseWriter, r *http.Request) bool {
	if r == nil || r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	limit := int64(maxJSONRequestBodyBytes)
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		limit = maxMultipartRequestBodyBytes
		if r.URL.Path == "/api/external-image-tasks/generations" {
			limit = maxExternalMultipartBytes
		}
	}
	if r.ContentLength > limit {
		applyCORS(w, r)
		util.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return true
}

func (a *App) logHTTPRequest(r *http.Request, status int, duration time.Duration) {
	if a.logger == nil {
		return
	}
	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"duration_ms", duration.Milliseconds(),
		"ip_address", clientIP(r),
	}
	switch {
	case status >= http.StatusInternalServerError:
		a.logger.Error("http request", attrs...)
	case status >= http.StatusBadRequest:
		a.logger.Warning("http request", attrs...)
	default:
		a.logger.Debug("http request", attrs...)
	}
}

func (a *App) writeAuditLog(r *http.Request, recorder *auditResponseWriter, status int, duration time.Duration, requestCapture auditRequestCapture) {
	if a.logs == nil {
		return
	}
	detail := map[string]any{
		"method":         r.Method,
		"path":           r.URL.Path,
		"module":         inferAuditModule(r.URL.Path),
		"status":         status,
		"duration_ms":    duration.Milliseconds(),
		"ip_address":     clientIP(r),
		"user_agent":     r.UserAgent(),
		"operation_type": operationTypeForMethod(r.Method),
		"log_level":      logLevelForStatus(status),
	}
	addAuditRequestDetail(detail, requestCapture)
	if r.URL.Path != "/api/external-chat/completions" && r.URL.Path != "/api/external-chat-conversations" {
		if responseBody := normalizeAuditPayload(recorder.body.Bytes()); responseBody != nil {
			detail["response_body"] = responseBody
		}
	}
	if identity, ok := requestIdentity(r.Context()); ok {
		addIdentityLogDetail(detail, identity)
		if name := identityDisplayName(identity); name != "" {
			detail["username"] = name
		}
	} else {
		detail["username"] = "anonymous"
	}

	if err := a.logs.Add(strings.TrimSpace(r.Method+" "+r.URL.Path), detail); err != nil && a.logger != nil {
		a.logger.Error("create audit log failed", "error", err, "path", r.URL.Path, "method", r.Method)
	}
}

func withRequestIdentity(ctx context.Context, identity service.Identity) context.Context {
	return context.WithValue(ctx, requestIdentityContextKey{}, identity)
}

func requestIdentity(ctx context.Context) (service.Identity, bool) {
	identity, ok := ctx.Value(requestIdentityContextKey{}).(service.Identity)
	return identity, ok
}

func withAuditRequestCapture(ctx context.Context, capture auditRequestCapture) context.Context {
	return context.WithValue(ctx, auditRequestContextKey{}, capture)
}

func requestAuditCapture(ctx context.Context) auditRequestCapture {
	capture, _ := ctx.Value(auditRequestContextKey{}).(auditRequestCapture)
	return capture
}

func markRequestBusinessLogged(r *http.Request) {
	if r == nil {
		return
	}
	*r = *r.WithContext(context.WithValue(r.Context(), businessLogContextKey{}, true))
}

func requestBusinessLogged(ctx context.Context) bool {
	value, _ := ctx.Value(businessLogContextKey{}).(bool)
	return value
}

func requestStartedAt(ctx context.Context) time.Time {
	startedAt, _ := ctx.Value(requestStartedContextKey{}).(time.Time)
	if startedAt.IsZero() {
		return time.Now()
	}
	return startedAt
}

func addAuditRequestDetail(detail map[string]any, capture auditRequestCapture) {
	if detail == nil {
		return
	}
	if capture.args != nil {
		detail["request_args"] = capture.args
	}
	if capture.truncated {
		detail["request_truncated"] = true
	}
}

func shouldWriteAuditLog(r *http.Request, status int) bool {
	if r == nil {
		return true
	}
	if requestBusinessLogged(r.Context()) {
		return false
	}
	if status >= http.StatusBadRequest {
		return true
	}
	return !isNoisySuccessfulAuditRequest(r)
}

func isNoisySuccessfulAuditRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	path := r.URL.Path
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		switch {
		case path == "/api/logs",
			path == "/api/logs/governance",
			path == "/api/images/storage-governance",
			path == "/api/creation-tasks",
			path == "/api/external-image-tasks",
			path == "/api/external-chat-conversations",
			path == "/api/app-meta",
			path == "/api/admin/permissions",
			path == "/auth/session":
			return true
		}
	}
	return false
}

func captureAuditRequest(r *http.Request) auditRequestCapture {
	if r == nil {
		return auditRequestCapture{}
	}
	query := captureAuditQuery(r)
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		return auditRequestCapture{args: combineAuditArgs(query, "[multipart/form-data]")}
	}
	if r.Method != http.MethodGet && r.Body != nil {
		body, truncated, ok := captureAuditBody(r)
		if ok {
			if r.URL.Path == "/api/external-chat/completions" {
				return auditRequestCapture{args: combineAuditArgs(query, externalChatAuditPayload(body)), truncated: truncated}
			}
			if r.URL.Path == "/api/external-chat-conversations" {
				return auditRequestCapture{args: combineAuditArgs(query, externalChatHistoryAuditPayload(body)), truncated: truncated}
			}
			if bodyPayload := normalizeAuditPayload(body); bodyPayload != nil {
				return auditRequestCapture{args: combineAuditArgs(query, bodyPayload), truncated: truncated}
			}
		}
	}
	return auditRequestCapture{args: query}
}

func externalChatAuditPayload(raw []byte) any {
	payload := map[string]any{"messages": "[REDACTED]"}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil {
		return payload
	}
	for _, key := range []string{"provider_id", "model", "temperature", "max_completion_tokens"} {
		if value, ok := decoded[key]; ok {
			payload[key] = value
		}
	}
	messages := util.AsMapSlice(decoded["messages"])
	payload["message_count"] = len(messages)
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		if role := util.Clean(message["role"]); role != "" {
			roles = append(roles, role)
		}
	}
	payload["roles"] = roles
	return service.SanitizeLogValue(payload)
}

func externalChatHistoryAuditPayload(raw []byte) any {
	payload := map[string]any{
		"conversation_count": 0,
		"message_count":      0,
	}
	var decoded struct {
		Items []struct {
			Messages []json.RawMessage `json:"messages"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &decoded) != nil {
		return payload
	}
	payload["conversation_count"] = len(decoded.Items)
	messageCount := 0
	for _, item := range decoded.Items {
		messageCount += len(item.Messages)
	}
	payload["message_count"] = messageCount
	return payload
}

func captureAuditBody(r *http.Request) ([]byte, bool, bool) {
	if r == nil || r.Body == nil {
		return nil, false, true
	}
	captured, err := io.ReadAll(io.LimitReader(r.Body, int64(maxAuditRequestPayloadBytes)+1))
	r.Body = &auditBodyReadCloser{Reader: io.MultiReader(bytes.NewReader(captured), r.Body), closer: r.Body}
	if err != nil {
		return nil, false, false
	}
	if len(captured) > maxAuditRequestPayloadBytes {
		return captured[:maxAuditRequestPayloadBytes], true, true
	}
	return captured, false, true
}

func captureAuditQuery(r *http.Request) any {
	if r == nil || r.URL == nil {
		return nil
	}
	if strings.TrimSpace(r.URL.RawQuery) == "" {
		return nil
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return service.SanitizeLogValue(r.URL.RawQuery)
	}
	payload := make(map[string]any, len(values))
	for key, items := range values {
		if len(items) == 1 {
			payload[key] = items[0]
			continue
		}
		payload[key] = items
	}
	return service.SanitizeLogValue(payload)
}

func combineAuditArgs(query, body any) any {
	if query == nil {
		return body
	}
	if body == nil {
		return query
	}
	return map[string]any{"query": query, "body": body}
}

func normalizeAuditPayload(raw []byte) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if len(trimmed) > maxAuditRequestPayloadBytes {
		trimmed = append([]byte(nil), trimmed[:maxAuditRequestPayloadBytes]...)
	}
	if json.Valid(trimmed) {
		var decoded any
		if err := json.Unmarshal(trimmed, &decoded); err == nil {
			return service.SanitizeLogValue(decoded)
		}
	}
	return service.SanitizeLogValue(string(trimmed))
}

func operationTypeForMethod(method string) string {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet:
		return "查询"
	case http.MethodPost:
		return "提交"
	case http.MethodPut, http.MethodPatch:
		return "更新"
	case http.MethodDelete:
		return "删除"
	default:
		return "操作"
	}
}

func logLevelForStatus(status int) string {
	switch {
	case status >= http.StatusInternalServerError:
		return "error"
	case status >= http.StatusBadRequest:
		return "warning"
	default:
		return "info"
	}
}

func inferAuditModule(path string) string {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return "system"
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 2 {
		return parts[1]
	}
	return parts[0]
}

func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	peer := remoteAddressIP(r.RemoteAddr)
	if !trustedProxyPeer(peer) {
		return peer
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		for index := len(parts) - 1; index >= 0; index-- {
			candidate := strings.TrimSpace(parts[index])
			if net.ParseIP(candidate) == nil {
				continue
			}
			if !trustedProxyPeer(candidate) {
				return candidate
			}
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(realIP) != nil {
		return realIP
	}
	return peer
}

func remoteAddressIP(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return util.Clean(remoteAddr)
}

func trustedProxyPeer(peer string) bool {
	ip := net.ParseIP(strings.TrimSpace(peer))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, value := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func parseLogQuery(r *http.Request) (service.LogQuery, error) {
	values := r.URL.Query()
	limit, err := parseLogPageSize(values.Get("page_size"))
	if err != nil {
		return service.LogQuery{}, err
	}
	return service.LogQuery{
		Username:      strings.TrimSpace(values.Get("username")),
		Module:        strings.TrimSpace(values.Get("module")),
		Method:        strings.TrimSpace(values.Get("method")),
		Summary:       strings.TrimSpace(values.Get("summary")),
		Status:        strings.TrimSpace(values.Get("status")),
		IPAddress:     strings.TrimSpace(values.Get("ip_address")),
		OperationType: strings.TrimSpace(values.Get("operation_type")),
		LogLevel:      strings.TrimSpace(values.Get("log_level")),
		StartDate:     strings.TrimSpace(values.Get("start_date")),
		EndDate:       strings.TrimSpace(values.Get("end_date")),
		StartTime:     strings.TrimSpace(values.Get("start_time")),
		EndTime:       strings.TrimSpace(values.Get("end_time")),
		View:          strings.TrimSpace(values.Get("view")),
		Limit:         limit,
	}, nil
}

func parseLogPageSize(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 200, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("page_size 参数无效")
	}
	return normalizedHTTPLogPageSize(value), nil
}

func normalizedHTTPLogPageSize(limit int) int {
	if limit <= 0 {
		return 200
	}
	if limit > 500 {
		return 500
	}
	return limit
}
