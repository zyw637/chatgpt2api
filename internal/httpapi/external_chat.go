package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"chatgpt2api/internal/service"
	"chatgpt2api/internal/util"
)

func (a *App) handleExternalChatProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireIdentity(w, r, ""); !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	util.WriteJSON(w, http.StatusOK, map[string]any{"items": a.external.ListChatProviders()})
}

func (a *App) handleExternalChatConversations(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r, "")
	if !ok {
		return
	}
	ownerID := identityScope(identity)
	if ownerID == "" {
		util.WriteError(w, http.StatusForbidden, "external chat history requires a bound user account")
		return
	}
	w.Header().Set("Cache-Control", "no-store")

	switch r.Method {
	case http.MethodGet:
		document, err := a.chatHistory.List(ownerID)
		if err != nil {
			util.WriteError(w, http.StatusInternalServerError, "failed to load external chat history")
			return
		}
		util.WriteJSON(w, http.StatusOK, document)
	case http.MethodPut:
		document, err := readExternalChatHistory(r)
		if err != nil {
			writeRequestBodyError(w, err, "invalid external chat history")
			return
		}
		document, err = a.chatHistory.Sync(ownerID, document)
		if err != nil {
			var inputErr service.ExternalChatHistoryInputError
			if errors.As(err, &inputErr) {
				util.WriteError(w, http.StatusBadRequest, inputErr.Error())
				return
			}
			util.WriteError(w, http.StatusInternalServerError, "failed to save external chat history")
			return
		}
		util.WriteJSON(w, http.StatusOK, document)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func readExternalChatHistory(r *http.Request) (service.ExternalChatHistoryDocument, error) {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var document service.ExternalChatHistoryDocument
	if err := decoder.Decode(&document); err != nil {
		return service.ExternalChatHistoryDocument{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return service.ExternalChatHistoryDocument{}, errors.New("request body must contain one JSON object")
		}
		return service.ExternalChatHistoryDocument{}, err
	}
	if document.Items == nil {
		document.Items = []service.ExternalChatHistoryConversation{}
	}
	if document.Deletions == nil {
		document.Deletions = []service.ExternalChatHistoryDeletion{}
	}
	return document, nil
}

func (a *App) handleExternalChatCompletions(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r, "")
	if !ok {
		return
	}
	started := time.Now()
	submission, err := readExternalChatSubmission(r)
	if err != nil {
		a.logExternalChatRequestFailure(r, identity, submission, started, err)
		writeRequestBodyError(w, err, "invalid external chat request")
		return
	}
	stream, err := a.external.StartChat(r.Context(), identity, submission)
	if err != nil {
		a.logExternalChatRequestFailure(r, identity, submission, started, err)
		writeExternalChatError(w, err)
		return
	}
	defer stream.Body.Close()
	defer stream.Release()

	flusher, ok := w.(http.Flusher)
	if !ok {
		util.WriteError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", stream.ContentType)
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	if stream.RequestID != "" {
		w.Header().Set("X-Request-ID", stream.RequestID)
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	buffer := make([]byte, 32*1024)
	bytesForwarded := 0
	isEventStream := strings.Contains(strings.ToLower(stream.ContentType), "text/event-stream")
	for {
		count, readErr := stream.Body.Read(buffer)
		if count > 0 {
			written, writeErr := w.Write(buffer[:count])
			bytesForwarded += written
			if writeErr != nil {
				a.logExternalChatStreamIssue(r, submission, stream.RequestID, "client_write", writeErr, bytesForwarded)
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				if isEventStream {
					writeExternalChatStreamError(w, flusher)
				}
				a.logExternalChatStreamIssue(r, submission, stream.RequestID, "upstream_read", readErr, bytesForwarded)
			}
			return
		}
	}
}

func (a *App) logExternalChatRequestFailure(r *http.Request, identity service.Identity, submission service.ExternalChatSubmission, started time.Time, requestErr error) {
	a.logCall(r.Context(), identity, "API 聊天", http.MethodPost, "/api/external-chat/completions", submission.Model, started, "failed", externalChatErrorStatus(requestErr), requestErr.Error(), nil, requestAuditCapture(r.Context()))
	markRequestBusinessLogged(r)
}

func writeExternalChatStreamError(w http.ResponseWriter, flusher http.Flusher) {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]string{
			"message": "外部聊天上游流中断，已接收内容可能不完整",
		},
	})
	if err != nil {
		return
	}
	if _, err := w.Write([]byte("data: " + string(payload) + "\n\n")); err == nil {
		flusher.Flush()
	}
}

func (a *App) logExternalChatStreamIssue(r *http.Request, submission service.ExternalChatSubmission, requestID, phase string, streamErr error, bytesForwarded int) {
	if a == nil || a.logs == nil {
		return
	}
	detail := map[string]any{
		"method":          http.MethodPost,
		"path":            "/api/external-chat/completions",
		"module":          "external-chat",
		"status":          http.StatusBadGateway,
		"log_level":       "error",
		"provider_id":     submission.ProviderID,
		"model":           submission.Model,
		"phase":           phase,
		"bytes_forwarded": bytesForwarded,
		"error":           streamErr.Error(),
		"request_id":      requestID,
		"operation_type":  "提交",
		"message_count":   len(submission.Messages),
	}
	markRequestBusinessLogged(r)
	_ = a.logs.Add("API 聊天流中断", detail)
	if a.logger != nil {
		a.logger.Error("external chat stream interrupted", "phase", phase, "provider_id", submission.ProviderID, "model", submission.Model, "bytes_forwarded", bytesForwarded, "request_id", requestID, "error", streamErr)
	}
}

func readExternalChatSubmission(r *http.Request) (service.ExternalChatSubmission, error) {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var submission service.ExternalChatSubmission
	if err := decoder.Decode(&submission); err != nil {
		return service.ExternalChatSubmission{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return service.ExternalChatSubmission{}, errors.New("request body must contain one JSON object")
		}
		return service.ExternalChatSubmission{}, err
	}
	return submission, nil
}

func writeExternalChatError(w http.ResponseWriter, err error) {
	var limitErr service.ExternalImageTaskLimitError
	if errors.As(err, &limitErr) {
		util.WriteError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	var upstreamErr service.ExternalImageUpstreamError
	if errors.As(err, &upstreamErr) {
		message := strings.TrimSpace(upstreamErr.Message)
		if message == "" {
			message = strings.TrimSpace(upstreamErr.Detail)
		}
		if message == "" {
			message = "external chat upstream request failed"
		}
		util.WriteJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{
				"message":         message,
				"code":            upstreamErr.Code,
				"request_id":      upstreamErr.RequestID,
				"upstream_status": upstreamErr.Status,
			},
		})
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		util.WriteError(w, http.StatusGatewayTimeout, "external chat request was canceled")
		return
	}
	var transportErr service.ExternalChatTransportError
	if errors.As(err, &transportErr) {
		util.WriteError(w, http.StatusBadGateway, "external chat upstream connection failed")
		return
	}
	util.WriteError(w, http.StatusBadRequest, err.Error())
}

func externalChatErrorStatus(err error) int {
	var limitErr service.ExternalImageTaskLimitError
	if errors.As(err, &limitErr) {
		return http.StatusTooManyRequests
	}
	var upstreamErr service.ExternalImageUpstreamError
	if errors.As(err, &upstreamErr) {
		return http.StatusBadGateway
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	var transportErr service.ExternalChatTransportError
	if errors.As(err, &transportErr) {
		return http.StatusBadGateway
	}
	return http.StatusBadRequest
}
