package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"chatgpt2api/internal/util"
)

const (
	externalChatMaxMessages         = 200
	externalChatMaxContentRunes     = 500_000
	externalChatMaxUpstreamError    = 1 << 20
	externalChatMaxCompletionTokens = 131_072
)

type ExternalChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ExternalChatSubmission struct {
	ProviderID          string                `json:"provider_id"`
	Model               string                `json:"model"`
	Messages            []ExternalChatMessage `json:"messages"`
	Temperature         *float64              `json:"temperature,omitempty"`
	MaxCompletionTokens *int                  `json:"max_completion_tokens,omitempty"`
}

type ExternalChatStream struct {
	Body        io.ReadCloser
	ContentType string
	RequestID   string
	Release     func()
}

type ExternalChatTransportError struct {
	Err error
}

func (e ExternalChatTransportError) Error() string {
	return "external chat upstream transport failed: " + e.Err.Error()
}

func (e ExternalChatTransportError) Unwrap() error {
	return e.Err
}

func (s *ExternalImageService) StartChat(ctx context.Context, identity Identity, submission ExternalChatSubmission) (*ExternalChatStream, error) {
	if externalOwnerID(identity) == "" {
		return nil, errors.New("identity owner is required")
	}
	if err := normalizeExternalChatSubmission(&submission); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("external provider service is closed")
	}
	provider, ok := s.providerLocked(submission.ProviderID)
	if !ok || !provider.Enabled || !provider.ChatEnabled || provider.APIKey == "" {
		s.mu.Unlock()
		return nil, errors.New("external chat provider is unavailable")
	}
	if submission.Model == "" {
		submission.Model = provider.ChatDefaultModel
	}
	if !externalProviderHasChatModel(provider, submission.Model) {
		s.mu.Unlock()
		return nil, errors.New("model is not configured for the selected provider")
	}
	owner := externalOwnerID(identity)
	if err := s.checkChatLimitsLocked(owner, time.Now()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	sem := s.chatProviderSem[provider.ID]
	if sem == nil || cap(sem) != provider.ChatConcurrencyLimit {
		sem = make(chan struct{}, provider.ChatConcurrencyLimit)
		s.chatProviderSem[provider.ID] = sem
	}
	s.chatRunning[owner]++
	s.chatSubmitTimes[owner] = append(s.chatSubmitTimes[owner], time.Now())
	s.mu.Unlock()

	var releaseUserOnce sync.Once
	releaseUser := func() {
		releaseUserOnce.Do(func() {
			s.mu.Lock()
			if s.chatRunning[owner] > 0 {
				s.chatRunning[owner]--
			}
			s.mu.Unlock()
		})
	}

	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		releaseUser()
		return nil, ctx.Err()
	}
	var releaseStreamOnce sync.Once
	release := func() {
		releaseStreamOnce.Do(func() {
			<-sem
			releaseUser()
		})
	}

	payload := map[string]any{
		"model":    submission.Model,
		"messages": submission.Messages,
		"stream":   true,
	}
	if submission.Temperature != nil {
		payload["temperature"] = *submission.Temperature
	} else if provider.ChatTemperature != nil {
		payload["temperature"] = *provider.ChatTemperature
	}
	if submission.MaxCompletionTokens != nil {
		payload["max_completion_tokens"] = *submission.MaxCompletionTokens
	}
	data, err := json.Marshal(payload)
	if err != nil {
		release()
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.BaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		release()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := externalProviderAPIClient(provider).Do(req)
	if err != nil {
		release()
		return nil, ExternalChatTransportError{Err: err}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		upstreamErr := readExternalChatUpstreamError(resp, provider.APIKey)
		release()
		return nil, upstreamErr
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "text/event-stream; charset=utf-8"
	}
	return &ExternalChatStream{
		Body:        resp.Body,
		ContentType: contentType,
		RequestID:   strings.TrimSpace(resp.Header.Get("X-Request-ID")),
		Release:     release,
	}, nil
}

func normalizeExternalChatSubmission(submission *ExternalChatSubmission) error {
	submission.ProviderID = strings.TrimSpace(submission.ProviderID)
	submission.Model = strings.TrimSpace(submission.Model)
	if submission.ProviderID == "" {
		return errors.New("provider_id is required")
	}
	if len(submission.Messages) == 0 || len(submission.Messages) > externalChatMaxMessages {
		return errors.New("messages must contain between 1 and 200 items")
	}
	allowedRoles := map[string]struct{}{
		"system": {}, "developer": {}, "user": {}, "assistant": {},
	}
	totalRunes := 0
	messages := make([]ExternalChatMessage, 0, len(submission.Messages))
	for index := range submission.Messages {
		message := submission.Messages[index]
		message.Role = strings.TrimSpace(message.Role)
		if _, ok := allowedRoles[message.Role]; !ok {
			return errors.New("message role must be system, developer, user or assistant")
		}
		if strings.TrimSpace(message.Content) == "" {
			if message.Role == "assistant" {
				continue
			}
			return errors.New("message content is required")
		}
		totalRunes += len([]rune(message.Content))
		if totalRunes > externalChatMaxContentRunes {
			return errors.New("message content is too large")
		}
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return errors.New("messages must contain at least one non-empty item")
	}
	submission.Messages = messages
	if submission.Temperature != nil && (math.IsNaN(*submission.Temperature) || math.IsInf(*submission.Temperature, 0) || *submission.Temperature < 0 || *submission.Temperature > 2) {
		return errors.New("temperature must be between 0 and 2")
	}
	if submission.MaxCompletionTokens != nil && (*submission.MaxCompletionTokens < 1 || *submission.MaxCompletionTokens > externalChatMaxCompletionTokens) {
		return errors.New("max_completion_tokens must be between 1 and 131072")
	}
	return nil
}

func externalProviderHasChatModel(provider ExternalImageProvider, model string) bool {
	for _, item := range provider.ChatModels {
		if item == model {
			return true
		}
	}
	return false
}

func (s *ExternalImageService) checkChatLimitsLocked(owner string, now time.Time) error {
	settings := normalizeExternalImageSettings(s.settings)
	if s.chatRunning[owner] >= settings.ChatUserConcurrentLimit {
		return ExternalImageTaskLimitError{Message: "external chat concurrent request limit exceeded"}
	}
	cutoff := now.Add(-time.Minute)
	kept := s.chatSubmitTimes[owner][:0]
	for _, submitted := range s.chatSubmitTimes[owner] {
		if submitted.After(cutoff) {
			kept = append(kept, submitted)
		}
	}
	s.chatSubmitTimes[owner] = kept
	if len(kept) >= settings.ChatUserRPMLimit {
		return ExternalImageTaskLimitError{Message: "external chat RPM limit exceeded"}
	}
	return nil
}

func readExternalChatUpstreamError(resp *http.Response, apiKey string) error {
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, externalChatMaxUpstreamError+1))
	if readErr != nil {
		return ExternalChatTransportError{Err: readErr}
	}
	if len(body) > externalChatMaxUpstreamError {
		body = body[:externalChatMaxUpstreamError]
	}
	upstreamErr := ExternalImageUpstreamError{Status: resp.StatusCode, Detail: externalErrorDetail(body, apiKey)}
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		errorBody, _ := payload["error"].(map[string]any)
		upstreamErr.Code = util.Clean(errorBody["code"])
		upstreamErr.Message = util.Clean(errorBody["message"])
		upstreamErr.RequestID = util.Clean(errorBody["request_id"])
		if upstreamErr.RequestID == "" {
			upstreamErr.RequestID = strings.TrimSpace(resp.Header.Get("X-Request-ID"))
		}
	}
	return upstreamErr
}
