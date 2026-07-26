package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatgpt2api/internal/storage"
	"chatgpt2api/internal/util"

	_ "github.com/HugoSmits86/nativewebp"
)

const (
	ExternalImageProtocolImages = "openai_images"
	ExternalImageProtocolEdits  = "openai_image_edits"
	ExternalImageProtocolChat   = "openai_chat_images"
	ExternalChatProtocolOpenAI  = "openai_chat"

	externalImageProvidersDocument = "external_image_providers.json"
	externalImageTasksDocument     = "external_image_tasks.json"
	externalImageMaxDownloadBytes  = 25 << 20
	externalImageMaxReferenceBytes = 10 << 20
	externalImageMaxReferences     = 4
	externalImageMaxDimension      = 32768
	externalImageMaxPixels         = 100_000_000
	externalImageMaxPromptRunes    = 12_000
	externalImageMaxTasksPerOwner  = 100
	externalImageTaskRetention     = 30 * 24 * time.Hour
)

var (
	externalDataURLRE       = regexp.MustCompile(`data:(image/[a-zA-Z0-9.+-]+);base64,([a-zA-Z0-9+/=\r\n]+)`)
	externalMarkdownImageRE = regexp.MustCompile(`!\[[^\]]*\]\((https?://[^\s)]+)\)`)
)

type ExternalImageConfig interface {
	ExternalImageReferencesDir() string
}

type ExternalImageProvider struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Enabled              bool     `json:"enabled"`
	SortOrder            int      `json:"sort_order"`
	ImageEnabled         bool     `json:"image_enabled"`
	Protocol             string   `json:"protocol"`
	BaseURL              string   `json:"base_url"`
	APIKey               string   `json:"api_key"`
	Models               []string `json:"models"`
	DefaultModel         string   `json:"default_model"`
	DefaultSize          string   `json:"default_size"`
	DefaultQuality       string   `json:"default_quality"`
	Temperature          float64  `json:"temperature"`
	MaxImages            int      `json:"max_images"`
	MaxReferenceImages   int      `json:"max_reference_images"`
	TimeoutSeconds       int      `json:"timeout_seconds"`
	ConcurrencyLimit     int      `json:"concurrency_limit"`
	ChatEnabled          bool     `json:"chat_enabled"`
	ChatProtocol         string   `json:"chat_protocol"`
	ChatModels           []string `json:"chat_models"`
	ChatDefaultModel     string   `json:"chat_default_model"`
	ChatTemperature      *float64 `json:"chat_temperature"`
	ChatConcurrencyLimit int      `json:"chat_concurrency_limit"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
}

type ExternalImageSettings struct {
	UserConcurrentLimit     int `json:"user_concurrent_limit"`
	UserRPMLimit            int `json:"user_rpm_limit"`
	ChatUserConcurrentLimit int `json:"chat_user_concurrent_limit"`
	ChatUserRPMLimit        int `json:"chat_user_rpm_limit"`
}

type ExternalImageReferenceUpload struct {
	Name        string
	ContentType string
	Data        []byte
}

type ExternalImageSubmission struct {
	ClientTaskID string
	RetryOf      string
	ProviderID   string
	Model        string
	Prompt       string
	N            int
	Size         string
	AspectRatio  string
	Quality      string
	Temperature  *float64
	References   []ExternalImageReferenceUpload
}

type ExternalImageTaskLimitError struct {
	Message string
}

type ExternalImageTaskConflictError struct {
	Message string
}

type ExternalImageTaskPersistenceError struct {
	Operation string
	Err       error
}

func (e ExternalImageTaskPersistenceError) Error() string {
	if e.Operation == "" {
		return fmt.Sprintf("external image task persistence failed: %v", e.Err)
	}
	return fmt.Sprintf("external image task %s persistence failed: %v", e.Operation, e.Err)
}

func (e ExternalImageTaskPersistenceError) Unwrap() error { return e.Err }

type ExternalImageProviderPersistenceError struct {
	Operation string
	Err       error
}

func (e ExternalImageProviderPersistenceError) Error() string {
	return fmt.Sprintf("external image provider %s persistence failed: %v", e.Operation, e.Err)
}

func (e ExternalImageProviderPersistenceError) Unwrap() error { return e.Err }

func (e ExternalImageTaskConflictError) Error() string {
	return e.Message
}

func (e ExternalImageTaskLimitError) Error() string {
	return e.Message
}

type ExternalImageUpstreamError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Detail    string
}

func (e ExternalImageUpstreamError) Error() string {
	return fmt.Sprintf("external image upstream status=%d detail=%s", e.Status, e.Detail)
}

func (e ExternalImageUpstreamError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == http.StatusBadGateway || e.Status == http.StatusServiceUnavailable || e.Status == http.StatusGatewayTimeout
}

type externalImageProviderDocument struct {
	Items    []ExternalImageProvider `json:"items"`
	Settings ExternalImageSettings   `json:"settings"`
}

type externalImageResult struct {
	Data          []byte
	ContentType   string
	RevisedPrompt string
}

type ExternalImageService struct {
	mu              sync.Mutex
	workers         sync.WaitGroup
	store           storage.JSONDocumentBackend
	taskStore       storage.ExternalImageTaskBackend
	config          ExternalImageConfig
	images          *ImageService
	logger          *Logger
	providers       []ExternalImageProvider
	settings        ExternalImageSettings
	tasks           map[string]map[string]any
	cancels         map[string]context.CancelFunc
	submitting      map[string]chan struct{}
	running         map[string]int
	submitTimes     map[string][]time.Time
	providerSem     map[string]chan struct{}
	chatRunning     map[string]int
	chatSubmitTimes map[string][]time.Time
	chatProviderSem map[string]chan struct{}
	closed          bool
	initErr         error
}

func NewExternalImageService(backend storage.Backend, config ExternalImageConfig, images *ImageService, logger *Logger) *ExternalImageService {
	s := &ExternalImageService{
		store:           jsonDocumentStoreFromBackend(backend),
		taskStore:       externalImageTaskStoreFromBackend(backend),
		config:          config,
		images:          images,
		logger:          logger,
		tasks:           map[string]map[string]any{},
		cancels:         map[string]context.CancelFunc{},
		submitting:      map[string]chan struct{}{},
		running:         map[string]int{},
		submitTimes:     map[string][]time.Time{},
		providerSem:     map[string]chan struct{}{},
		chatRunning:     map[string]int{},
		chatSubmitTimes: map[string][]time.Time{},
		chatProviderSem: map[string]chan struct{}{},
	}
	s.initErr = s.load()
	return s
}

func (s *ExternalImageService) InitializationError() error {
	return s.initErr
}

func (s *ExternalImageService) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var providerDoc externalImageProviderDocument
	legacyCapabilities := map[int]bool{}
	if s.store == nil {
		return errors.New("external image document backend is required")
	}
	if s.taskStore == nil {
		return errors.New("external image task backend is required")
	}
	providerRaw, err := s.store.LoadJSONDocument(externalImageProvidersDocument)
	if err != nil {
		return fmt.Errorf("load external image providers: %w", err)
	}
	if providerRaw != nil {
		data, err := json.Marshal(providerRaw)
		if err != nil {
			return fmt.Errorf("encode stored external image providers: %w", err)
		}
		if err := json.Unmarshal(data, &providerDoc); err != nil {
			return fmt.Errorf("decode stored external image providers: %w", err)
		}
		var document struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if json.Unmarshal(data, &document) == nil {
			for index, item := range document.Items {
				_, hasImageEnabled := item["image_enabled"]
				_, hasChatEnabled := item["chat_enabled"]
				legacyCapabilities[index] = !hasImageEnabled && !hasChatEnabled
			}
		}
	}
	providerDoc.Settings = normalizeExternalImageSettings(providerDoc.Settings)
	migratedProviders := false
	migrationSafeToPersist := true
	for index, provider := range providerDoc.Items {
		if legacyCapabilities[index] {
			provider.ImageEnabled = true
			migratedProviders = true
		}
		normalized, err := normalizeExternalImageProvider(provider, false)
		if err != nil {
			migrationSafeToPersist = false
			return fmt.Errorf("normalize stored external image provider %d (%s): %w", index+1, provider.ID, err)
		}
		s.providers = append(s.providers, normalized)
	}
	s.settings = providerDoc.Settings
	if migratedProviders && migrationSafeToPersist {
		if err := s.saveProvidersLocked(); err != nil {
			return fmt.Errorf("persist external provider migration: %w", err)
		}
	}
	if err := s.migrateLegacyTaskDocumentLocked(); err != nil {
		return err
	}
	storedTasks, err := s.taskStore.LoadExternalImageTasks()
	if err != nil {
		return fmt.Errorf("load external image task records: %w", err)
	}
	recoveredKeys := make([]string, 0)
	for key, task := range storedTasks {
		owner := util.Clean(task["owner_id"])
		id := util.Clean(task["id"])
		if owner == "" || id == "" {
			return errors.New("load external image task records: task owner_id and id are required")
		}
		if expectedKey := externalTaskKey(owner, id); key != expectedKey {
			return fmt.Errorf("load external image task records: key %q does not match task owner and id", key)
		}
		status := util.Clean(task["status"])
		if status == TaskStatusQueued || status == TaskStatusRunning {
			task["status"] = TaskStatusError
			task["error"] = "服务重启，未完成的外部生图任务已终止"
			task["updated_at"] = util.NowISO()
			recoveredKeys = append(recoveredKeys, key)
		}
		s.tasks[key] = task
	}
	removed := s.pruneExpiredTasksLocked(time.Now().UTC())
	for _, key := range recoveredKeys {
		if _, pruned := removed[key]; pruned {
			continue
		}
		if err := s.persistTaskLocked(key); err != nil {
			s.restorePrunedTasksLocked(removed)
			return fmt.Errorf("persist recovered external image task %s: %w", key, err)
		}
	}
	if err := s.deleteTaskRecordsLocked(removed); err != nil {
		s.restorePrunedTasksLocked(removed)
		return fmt.Errorf("delete expired external image task records: %w", err)
	}
	s.cleanupPrunedTaskReferences(removed)
	s.migrateLegacyImagesLocked()
	s.cleanupOrphanedReferences()
	return nil
}

func normalizeExternalImageSettings(settings ExternalImageSettings) ExternalImageSettings {
	if settings.UserConcurrentLimit <= 0 {
		settings.UserConcurrentLimit = 2
	}
	if settings.UserRPMLimit <= 0 {
		settings.UserRPMLimit = 10
	}
	if settings.ChatUserConcurrentLimit <= 0 {
		settings.ChatUserConcurrentLimit = 2
	}
	if settings.ChatUserRPMLimit <= 0 {
		settings.ChatUserRPMLimit = 20
	}
	if settings.UserConcurrentLimit > 20 {
		settings.UserConcurrentLimit = 20
	}
	if settings.UserRPMLimit > 600 {
		settings.UserRPMLimit = 600
	}
	if settings.ChatUserConcurrentLimit > 20 {
		settings.ChatUserConcurrentLimit = 20
	}
	if settings.ChatUserRPMLimit > 600 {
		settings.ChatUserRPMLimit = 600
	}
	return settings
}

func normalizeExternalImageProvider(provider ExternalImageProvider, requireKey bool) (ExternalImageProvider, error) {
	provider.ID = strings.TrimSpace(provider.ID)
	if provider.ID == "" {
		provider.ID = "provider_" + util.NewHex(8)
	}
	provider.Name = strings.TrimSpace(provider.Name)
	if provider.Name == "" {
		return ExternalImageProvider{}, errors.New("provider name is required")
	}
	if provider.SortOrder < 0 || provider.SortOrder > 999 {
		return ExternalImageProvider{}, errors.New("sort_order must be between 0 and 999")
	}
	baseURL, err := normalizeExternalImageBaseURL(provider.BaseURL)
	if err != nil {
		return ExternalImageProvider{}, err
	}
	provider.BaseURL = baseURL
	provider.APIKey = strings.TrimSpace(provider.APIKey)
	if requireKey && provider.APIKey == "" {
		return ExternalImageProvider{}, errors.New("api_key is required")
	}
	if !provider.ImageEnabled && !provider.ChatEnabled {
		return ExternalImageProvider{}, errors.New("at least one provider capability must be enabled")
	}
	if provider.ImageEnabled {
		if err := normalizeExternalImageCapability(&provider); err != nil {
			return ExternalImageProvider{}, err
		}
	}
	if provider.ChatEnabled {
		if err := normalizeExternalChatCapability(&provider); err != nil {
			return ExternalImageProvider{}, err
		}
	}
	if provider.TimeoutSeconds == 0 {
		provider.TimeoutSeconds = 300
	}
	if provider.TimeoutSeconds < 30 || provider.TimeoutSeconds > 600 {
		return ExternalImageProvider{}, errors.New("timeout_seconds must be between 30 and 600")
	}
	return provider, nil
}

func normalizeExternalImageCapability(provider *ExternalImageProvider) error {
	provider.Protocol = strings.TrimSpace(provider.Protocol)
	if provider.Protocol != ExternalImageProtocolImages && provider.Protocol != ExternalImageProtocolEdits && provider.Protocol != ExternalImageProtocolChat {
		return errors.New("unsupported external image protocol")
	}
	seen := map[string]struct{}{}
	models := make([]string, 0, len(provider.Models))
	for _, model := range provider.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		return errors.New("at least one image model is required")
	}
	provider.Models = models
	provider.DefaultModel = strings.TrimSpace(provider.DefaultModel)
	if provider.DefaultModel == "" {
		provider.DefaultModel = models[0]
	}
	if _, ok := seen[provider.DefaultModel]; !ok {
		return errors.New("default_model must be included in models")
	}
	if math.IsNaN(provider.Temperature) || math.IsInf(provider.Temperature, 0) || provider.Temperature < 0 || provider.Temperature > 2 {
		return errors.New("temperature must be between 0 and 2")
	}
	if provider.MaxImages == 0 {
		provider.MaxImages = 4
	}
	if provider.MaxImages < 1 || provider.MaxImages > 4 {
		return errors.New("max_images must be between 1 and 4")
	}
	provider.DefaultSize = strings.TrimSpace(provider.DefaultSize)
	if provider.DefaultSize == "" {
		provider.DefaultSize = "auto"
	}
	if !externalImageSizeAllowed(provider.DefaultSize) {
		return errors.New("unsupported default_size")
	}
	provider.DefaultQuality = strings.TrimSpace(provider.DefaultQuality)
	if provider.DefaultQuality == "" {
		provider.DefaultQuality = "auto"
	}
	if !externalImageQualityAllowed(provider.DefaultQuality) {
		return errors.New("unsupported default_quality")
	}
	if provider.Protocol == ExternalImageProtocolImages {
		provider.MaxReferenceImages = 0
	} else {
		if provider.MaxReferenceImages == 0 {
			provider.MaxReferenceImages = externalImageMaxReferences
		}
		if provider.MaxReferenceImages < 1 || provider.MaxReferenceImages > externalImageMaxReferences {
			return fmt.Errorf("max_reference_images must be between 1 and %d", externalImageMaxReferences)
		}
	}
	if provider.ConcurrencyLimit == 0 {
		provider.ConcurrencyLimit = 2
	}
	if provider.ConcurrencyLimit < 1 || provider.ConcurrencyLimit > 20 {
		return errors.New("concurrency_limit must be between 1 and 20")
	}
	return nil
}

func normalizeExternalChatCapability(provider *ExternalImageProvider) error {
	provider.ChatProtocol = strings.TrimSpace(provider.ChatProtocol)
	if provider.ChatProtocol == "" {
		provider.ChatProtocol = ExternalChatProtocolOpenAI
	}
	if provider.ChatProtocol != ExternalChatProtocolOpenAI {
		return errors.New("unsupported external chat protocol")
	}
	seen := map[string]struct{}{}
	models := make([]string, 0, len(provider.ChatModels))
	for _, model := range provider.ChatModels {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		return errors.New("at least one chat model is required")
	}
	provider.ChatModels = models
	provider.ChatDefaultModel = strings.TrimSpace(provider.ChatDefaultModel)
	if provider.ChatDefaultModel == "" {
		provider.ChatDefaultModel = models[0]
	}
	if _, ok := seen[provider.ChatDefaultModel]; !ok {
		return errors.New("chat_default_model must be included in chat_models")
	}
	if provider.ChatTemperature != nil && (math.IsNaN(*provider.ChatTemperature) || math.IsInf(*provider.ChatTemperature, 0) || *provider.ChatTemperature < 0 || *provider.ChatTemperature > 2) {
		return errors.New("chat_temperature must be between 0 and 2")
	}
	if provider.ChatConcurrencyLimit == 0 {
		provider.ChatConcurrencyLimit = 2
	}
	if provider.ChatConcurrencyLimit < 1 || provider.ChatConcurrencyLimit > 20 {
		return errors.New("chat_concurrency_limit must be between 1 and 20")
	}
	return nil
}

func externalImageSizeAllowed(value string) bool {
	switch strings.TrimSpace(value) {
	case "auto", "1024x1024", "1536x1024", "1024x1536":
		return true
	default:
		return false
	}
}

func externalImageQualityAllowed(value string) bool {
	switch strings.TrimSpace(value) {
	case "auto", "low", "medium", "high":
		return true
	default:
		return false
	}
}

func normalizeExternalImageBaseURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return "", errors.New("base_url must be an absolute URL")
	}
	if u.User != nil {
		return "", errors.New("base_url must not include user credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("base_url must not include query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isExternalLoopback(u.Hostname())) {
		return "", errors.New("base_url must use HTTPS except for loopback development")
	}
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
		return "", errors.New("base_url must end with /v1")
	}
	return value, nil
}

func isExternalLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func externalProviderPublic(provider ExternalImageProvider, admin bool) map[string]any {
	out := map[string]any{
		"id": provider.ID, "name": provider.Name, "enabled": provider.Enabled,
		"image_enabled": provider.ImageEnabled, "chat_enabled": provider.ChatEnabled,
		"protocol": provider.Protocol, "models": externalPublicStringList(provider.Models),
		"default_model": provider.DefaultModel, "default_size": provider.DefaultSize,
		"default_quality": provider.DefaultQuality, "temperature": provider.Temperature,
		"max_images": provider.MaxImages, "max_reference_images": provider.MaxReferenceImages,
		"chat_protocol": provider.ChatProtocol, "chat_models": externalPublicStringList(provider.ChatModels),
		"chat_default_model": provider.ChatDefaultModel, "chat_temperature": provider.ChatTemperature,
	}
	if admin {
		out["sort_order"] = provider.SortOrder
		out["base_url"] = provider.BaseURL
		out["has_api_key"] = provider.APIKey != ""
		out["temperature"] = provider.Temperature
		out["timeout_seconds"] = provider.TimeoutSeconds
		out["concurrency_limit"] = provider.ConcurrencyLimit
		out["chat_concurrency_limit"] = provider.ChatConcurrencyLimit
		out["created_at"] = provider.CreatedAt
		out["updated_at"] = provider.UpdatedAt
	}
	return out
}

func externalPublicStringList(items []string) []string {
	if len(items) == 0 {
		return []string{}
	}
	return append([]string(nil), items...)
}

func (s *ExternalImageService) ListProviders(admin bool) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.providers))
	for _, provider := range s.providers {
		if !admin && (!provider.Enabled || !provider.ImageEnabled || provider.APIKey == "") {
			continue
		}
		out = append(out, externalProviderPublic(provider, admin))
	}
	sort.SliceStable(out, func(i, j int) bool {
		leftID := util.Clean(out[i]["id"])
		rightID := util.Clean(out[j]["id"])
		leftIndex := s.providerIndexLocked(leftID)
		rightIndex := s.providerIndexLocked(rightID)
		leftOrder, rightOrder := 0, 0
		if leftIndex >= 0 {
			leftOrder = s.providers[leftIndex].SortOrder
		}
		if rightIndex >= 0 {
			rightOrder = s.providers[rightIndex].SortOrder
		}
		if leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return util.Clean(out[i]["name"]) < util.Clean(out[j]["name"])
	})
	return out
}

func externalChatProviderPublic(provider ExternalImageProvider) map[string]any {
	return map[string]any{
		"id":            provider.ID,
		"name":          provider.Name,
		"protocol":      provider.ChatProtocol,
		"models":        externalPublicStringList(provider.ChatModels),
		"default_model": provider.ChatDefaultModel,
		"temperature":   provider.ChatTemperature,
	}
}

func (s *ExternalImageService) ListChatProviders() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.providers))
	for _, provider := range s.providers {
		if !provider.Enabled || !provider.ChatEnabled || provider.APIKey == "" {
			continue
		}
		out = append(out, externalChatProviderPublic(provider))
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := s.providerIndexLocked(util.Clean(out[i]["id"]))
		right := s.providerIndexLocked(util.Clean(out[j]["id"]))
		leftOrder, rightOrder := 0, 0
		if left >= 0 {
			leftOrder = s.providers[left].SortOrder
		}
		if right >= 0 {
			rightOrder = s.providers[right].SortOrder
		}
		if leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return util.Clean(out[i]["name"]) < util.Clean(out[j]["name"])
	})
	return out
}

func (s *ExternalImageService) Settings() ExternalImageSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

func (s *ExternalImageService) Close() {
	s.CloseWithTimeout(0)
}

func (s *ExternalImageService) CloseWithTimeout(timeout time.Duration) bool {
	s.mu.Lock()
	s.closed = true
	cancels := make([]context.CancelFunc, 0, len(s.cancels))
	for _, cancel := range s.cancels {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return waitForServiceWorkers(&s.workers, timeout)
}

func (s *ExternalImageService) UpdateSettings(settings ExternalImageSettings) (ExternalImageSettings, error) {
	if settings.UserConcurrentLimit < 1 || settings.UserConcurrentLimit > 20 {
		return ExternalImageSettings{}, errors.New("user_concurrent_limit must be between 1 and 20")
	}
	if settings.UserRPMLimit < 1 || settings.UserRPMLimit > 600 {
		return ExternalImageSettings{}, errors.New("user_rpm_limit must be between 1 and 600")
	}
	if settings.ChatUserConcurrentLimit < 1 || settings.ChatUserConcurrentLimit > 20 {
		return ExternalImageSettings{}, errors.New("chat_user_concurrent_limit must be between 1 and 20")
	}
	if settings.ChatUserRPMLimit < 1 || settings.ChatUserRPMLimit > 600 {
		return ExternalImageSettings{}, errors.New("chat_user_rpm_limit must be between 1 and 600")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.settings
	s.settings = settings
	if err := s.saveProvidersLocked(); err != nil {
		s.settings = previous
		return ExternalImageSettings{}, ExternalImageProviderPersistenceError{Operation: "settings", Err: err}
	}
	return settings, nil
}

func (s *ExternalImageService) CreateProvider(provider ExternalImageProvider) (map[string]any, error) {
	now := util.NowISO()
	provider.ID = "provider_" + util.NewHex(8)
	provider.CreatedAt = now
	provider.UpdatedAt = now
	normalized, err := normalizeExternalImageProvider(provider, true)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.providers {
		if existing.ID == normalized.ID {
			return nil, errors.New("provider id already exists")
		}
	}
	previousLen := len(s.providers)
	s.providers = append(s.providers, normalized)
	if err := s.saveProvidersLocked(); err != nil {
		s.providers = s.providers[:previousLen]
		return nil, ExternalImageProviderPersistenceError{Operation: "create", Err: err}
	}
	return externalProviderPublic(normalized, true), nil
}

func (s *ExternalImageService) UpdateProvider(id string, updates map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.providerIndexLocked(id)
	if index < 0 {
		return nil, errors.New("external image provider not found")
	}
	current := s.providers[index]
	data, _ := json.Marshal(current)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	for key, value := range updates {
		if key == "id" || key == "created_at" || key == "updated_at" {
			continue
		}
		if key == "api_key" && strings.TrimSpace(util.Clean(value)) == "" {
			continue
		}
		raw[key] = value
	}
	data, _ = json.Marshal(raw)
	var next ExternalImageProvider
	if err := json.Unmarshal(data, &next); err != nil {
		return nil, err
	}
	next.ID = current.ID
	next.CreatedAt = current.CreatedAt
	next.UpdatedAt = util.NowISO()
	normalized, err := normalizeExternalImageProvider(next, true)
	if err != nil {
		return nil, err
	}
	s.providers[index] = normalized
	if err := s.saveProvidersLocked(); err != nil {
		s.providers[index] = current
		return nil, ExternalImageProviderPersistenceError{Operation: "update", Err: err}
	}
	delete(s.providerSem, id)
	delete(s.chatProviderSem, id)
	return externalProviderPublic(normalized, true), nil
}

func (s *ExternalImageService) DeleteProvider(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.providerIndexLocked(id)
	if index < 0 {
		return errors.New("external image provider not found")
	}
	previous := append([]ExternalImageProvider(nil), s.providers...)
	s.providers = append(s.providers[:index], s.providers[index+1:]...)
	if err := s.saveProvidersLocked(); err != nil {
		s.providers = previous
		return ExternalImageProviderPersistenceError{Operation: "delete", Err: err}
	}
	delete(s.providerSem, id)
	delete(s.chatProviderSem, id)
	return nil
}

func (s *ExternalImageService) providerIndexLocked(id string) int {
	for index := range s.providers {
		if s.providers[index].ID == strings.TrimSpace(id) {
			return index
		}
	}
	return -1
}

func (s *ExternalImageService) providerLocked(id string) (ExternalImageProvider, bool) {
	index := s.providerIndexLocked(id)
	if index < 0 {
		return ExternalImageProvider{}, false
	}
	return s.providers[index], true
}

func (s *ExternalImageService) saveProvidersLocked() error {
	return saveStoredJSON(s.store, externalImageProvidersDocument, externalImageProviderDocument{Items: s.providers, Settings: s.settings})
}

func (s *ExternalImageService) migrateLegacyTaskDocumentLocked() error {
	raw, err := s.store.LoadJSONDocument(externalImageTasksDocument)
	if err != nil {
		return fmt.Errorf("load legacy external image tasks: %w", err)
	}
	if raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encode legacy external image tasks: %w", err)
	}
	var tasks []map[string]any
	if err := json.Unmarshal(data, &tasks); err != nil {
		return fmt.Errorf("decode legacy external image tasks: %w", err)
	}
	for index, task := range tasks {
		owner := util.Clean(task["owner_id"])
		id := util.Clean(task["id"])
		if owner == "" || id == "" {
			return fmt.Errorf("decode legacy external image task %d: owner_id and id are required", index+1)
		}
		if err := s.taskStore.UpsertExternalImageTask(externalTaskKey(owner, id), util.CopyMap(task)); err != nil {
			return fmt.Errorf("migrate legacy external image task %s: %w", id, err)
		}
	}
	if err := s.store.DeleteJSONDocument(externalImageTasksDocument); err != nil {
		return fmt.Errorf("delete migrated external image task document: %w", err)
	}
	return nil
}

func (s *ExternalImageService) persistTaskLocked(key string) error {
	task := s.tasks[key]
	if task == nil {
		return errors.New("external image task not found")
	}
	stored := util.CopyMap(task)
	delete(stored, "persistence_error")
	if err := s.taskStore.UpsertExternalImageTask(key, stored); err != nil {
		return err
	}
	delete(task, "persistence_error")
	return nil
}

func (s *ExternalImageService) deleteTaskRecordsLocked(tasks map[string]map[string]any) error {
	if len(tasks) == 0 {
		return nil
	}
	keys := make([]string, 0, len(tasks))
	for key := range tasks {
		keys = append(keys, key)
	}
	return s.taskStore.DeleteExternalImageTasks(keys)
}

func (s *ExternalImageService) releaseSubmission(key string, done chan struct{}) {
	s.mu.Lock()
	if current := s.submitting[key]; current == done {
		delete(s.submitting, key)
		close(done)
	}
	s.mu.Unlock()
}

func (s *ExternalImageService) TestProvider(ctx context.Context, id string) (map[string]any, error) {
	s.mu.Lock()
	provider, ok := s.providerLocked(id)
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("external image provider not found")
	}
	client := externalProviderAPIClient(provider)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, provider.BaseURL+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Accept", "application/json")
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
			return nil, errors.New("该渠道不支持通过 /models 进行无费用连接测试，未发起生图请求")
		}
		return nil, fmt.Errorf("provider model test failed: status=%d detail=%s", resp.StatusCode, externalErrorDetail(body, provider.APIKey))
	}
	availableModels := make([]string, 0)
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		seen := map[string]struct{}{}
		for _, item := range util.AsMapSlice(payload["data"]) {
			model := util.Clean(item["id"])
			if model == "" {
				continue
			}
			if _, ok := seen[model]; ok {
				continue
			}
			seen[model] = struct{}{}
			availableModels = append(availableModels, model)
		}
	}
	sort.Strings(availableModels)
	availableSet := map[string]struct{}{}
	for _, model := range availableModels {
		availableSet[model] = struct{}{}
	}
	configuredModels := make([]string, 0, len(provider.Models)+len(provider.ChatModels))
	if provider.ImageEnabled {
		configuredModels = append(configuredModels, provider.Models...)
	}
	if provider.ChatEnabled {
		configuredModels = dedupeExternalStrings(append(configuredModels, provider.ChatModels...))
	}
	missingModels := make([]string, 0)
	if len(availableModels) > 0 {
		for _, model := range configuredModels {
			if _, ok := availableSet[model]; !ok {
				missingModels = append(missingModels, model)
			}
		}
	}
	return map[string]any{
		"ok": true, "status": resp.StatusCode, "latency_ms": time.Since(started).Milliseconds(),
		"available_models": availableModels, "missing_models": missingModels,
		"default_model_available":      !provider.ImageEnabled || len(availableModels) == 0 || !slicesContainString(missingModels, provider.DefaultModel),
		"chat_default_model_available": len(availableModels) == 0 || !slicesContainString(missingModels, provider.ChatDefaultModel),
	}, nil
}

func slicesContainString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func (s *ExternalImageService) Submit(identity Identity, submission ExternalImageSubmission) (map[string]any, error) {
	owner := externalOwnerID(identity)
	if owner == "" {
		return nil, errors.New("identity owner is required")
	}
	submission.ClientTaskID = strings.TrimSpace(submission.ClientTaskID)
	submission.RetryOf = strings.TrimSpace(submission.RetryOf)
	submission.ProviderID = strings.TrimSpace(submission.ProviderID)
	submission.Model = strings.TrimSpace(submission.Model)
	submission.Prompt = strings.TrimSpace(submission.Prompt)
	submission.AspectRatio = strings.TrimSpace(submission.AspectRatio)
	if submission.ClientTaskID == "" || submission.ProviderID == "" || submission.Prompt == "" {
		return nil, errors.New("client_task_id, provider_id and prompt are required")
	}
	if len([]rune(submission.Prompt)) > externalImageMaxPromptRunes {
		return nil, fmt.Errorf("prompt cannot exceed %d characters", externalImageMaxPromptRunes)
	}
	if submission.N < 1 || submission.N > 4 {
		return nil, errors.New("n must be between 1 and 4")
	}
	if submission.Temperature != nil && (math.IsNaN(*submission.Temperature) || math.IsInf(*submission.Temperature, 0) || *submission.Temperature < 0 || *submission.Temperature > 2) {
		return nil, errors.New("temperature must be between 0 and 2")
	}
	if submission.AspectRatio != "" && !externalImageCompositionRatioAllowed(submission.AspectRatio) {
		return nil, errors.New("unsupported composition ratio")
	}
	if len(submission.References) > externalImageMaxReferences {
		return nil, fmt.Errorf("at most %d reference images are allowed", externalImageMaxReferences)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("external image service is closed")
	}
	removed := s.pruneExpiredTasksLocked(time.Now().UTC())
	if len(removed) > 0 {
		if err := s.deleteTaskRecordsLocked(removed); err != nil {
			s.restorePrunedTasksLocked(removed)
			s.mu.Unlock()
			return nil, ExternalImageTaskPersistenceError{Operation: "retention", Err: err}
		}
		s.cleanupPrunedTaskReferences(removed)
	}
	ownerTaskCount := 0
	for _, task := range s.tasks {
		if util.Clean(task["owner_id"]) == owner {
			ownerTaskCount++
		}
	}
	if ownerTaskCount >= externalImageMaxTasksPerOwner {
		s.mu.Unlock()
		return nil, ExternalImageTaskLimitError{Message: fmt.Sprintf("external image task history limit reached (%d); delete old tasks before submitting", externalImageMaxTasksPerOwner)}
	}
	provider, ok := s.providerLocked(submission.ProviderID)
	if !ok || !provider.Enabled || !provider.ImageEnabled || provider.APIKey == "" {
		s.mu.Unlock()
		return nil, errors.New("external image provider is unavailable")
	}
	if submission.Model == "" {
		submission.Model = provider.DefaultModel
	}
	if !externalProviderHasModel(provider, submission.Model) {
		s.mu.Unlock()
		return nil, errors.New("model is not configured for the selected provider")
	}
	if submission.N > provider.MaxImages {
		s.mu.Unlock()
		return nil, fmt.Errorf("selected provider allows at most %d images per task", provider.MaxImages)
	}
	if submission.Size == "" {
		submission.Size = provider.DefaultSize
	}
	if submission.Quality == "" {
		submission.Quality = provider.DefaultQuality
	}
	if !externalImageRequestSizeAllowed(submission.Size) {
		s.mu.Unlock()
		return nil, errors.New("unsupported image size")
	}
	if !externalImageQualityAllowed(submission.Quality) {
		s.mu.Unlock()
		return nil, errors.New("unsupported image quality")
	}
	if provider.Protocol == ExternalImageProtocolImages && len(submission.References) > 0 {
		s.mu.Unlock()
		return nil, errors.New("reference images are not supported by Images Generations providers")
	}
	if provider.Protocol == ExternalImageProtocolEdits && len(submission.References) == 0 {
		s.mu.Unlock()
		return nil, errors.New("at least one reference image is required by Images Edits providers")
	}
	if provider.Protocol != ExternalImageProtocolImages && len(submission.References) > provider.MaxReferenceImages {
		s.mu.Unlock()
		return nil, fmt.Errorf("selected provider allows at most %d reference images", provider.MaxReferenceImages)
	}
	key := externalTaskKey(owner, submission.ClientTaskID)
	var submissionDone chan struct{}
	for {
		if existing := s.tasks[key]; existing != nil {
			result := externalPublicTask(existing)
			s.mu.Unlock()
			return result, nil
		}
		if waiting := s.submitting[key]; waiting != nil {
			s.mu.Unlock()
			<-waiting
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return nil, errors.New("external image service is closed")
			}
			continue
		}
		if err := s.checkLimitsLocked(owner, time.Now()); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		submissionDone = make(chan struct{})
		s.submitting[key] = submissionDone
		s.workers.Add(1)
		break
	}
	s.mu.Unlock()
	defer s.workers.Done()
	defer s.releaseSubmission(key, submissionDone)

	referencePaths, err := s.saveReferences(owner, submission.ClientTaskID, submission.References)
	if err != nil {
		return nil, err
	}
	now := util.NowISO()
	task := map[string]any{
		"id": submission.ClientTaskID, "owner_id": owner, "owner_name": identity.Name,
		"provider_id": provider.ID, "provider_name": provider.Name, "protocol": provider.Protocol,
		"model": submission.Model, "prompt": submission.Prompt, "n": submission.N,
		"size": submission.Size, "aspect_ratio": submission.AspectRatio, "quality": submission.Quality, "status": TaskStatusQueued,
		"data": []any{}, "created_at": now, "updated_at": now, "reference_paths": referencePaths,
		"reference_names": externalReferenceNames(submission.References), "reference_content_types": externalReferenceContentTypes(submission.References),
	}
	if submission.Temperature != nil {
		task["temperature"] = *submission.Temperature
	}
	if submission.RetryOf != "" {
		task["retry_of"] = submission.RetryOf
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(provider.TimeoutSeconds)*time.Second)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		s.cleanupReferences(referencePaths)
		return nil, errors.New("external image service is closed")
	}
	if existing := s.tasks[key]; existing != nil {
		s.mu.Unlock()
		cancel()
		s.cleanupReferences(referencePaths)
		return externalPublicTask(existing), nil
	}
	if err := s.checkLimitsLocked(owner, time.Now()); err != nil {
		s.mu.Unlock()
		cancel()
		s.cleanupReferences(referencePaths)
		return nil, err
	}
	s.tasks[key] = task
	s.cancels[key] = cancel
	s.running[owner]++
	s.submitTimes[owner] = append(s.submitTimes[owner], time.Now())
	if err := s.persistTaskLocked(key); err != nil {
		delete(s.tasks, key)
		delete(s.cancels, key)
		s.running[owner]--
		s.submitTimes[owner] = s.submitTimes[owner][:len(s.submitTimes[owner])-1]
		s.mu.Unlock()
		cancel()
		s.cleanupReferences(referencePaths)
		return nil, ExternalImageTaskPersistenceError{Operation: "create", Err: err}
	}
	result := externalPublicTask(task)
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		s.runTask(ctx, cancel, key, owner, identity.Name, provider, submission, referencePaths)
	}()
	return result, nil
}

func externalImageRequestSizeAllowed(value string) bool {
	value = strings.TrimSpace(value)
	if externalImageSizeAllowed(value) {
		return true
	}
	parts := strings.Split(strings.ToLower(value), "x")
	if len(parts) != 2 {
		return false
	}
	width, widthErr := strconv.Atoi(strings.TrimSpace(parts[0]))
	height, heightErr := strconv.Atoi(strings.TrimSpace(parts[1]))
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return false
	}
	return width <= externalImageMaxDimension && height <= externalImageMaxDimension && int64(width)*int64(height) <= externalImageMaxPixels
}

func externalImageCompositionRatioAllowed(value string) bool {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return false
	}
	width, widthErr := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	height, heightErr := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return false
	}
	ratio := width / height
	return ratio >= 0.1 && ratio <= 10
}

func externalImagePromptWithCompositionRatio(prompt, ratio string) string {
	ratio = strings.TrimSpace(ratio)
	if ratio == "" {
		return prompt
	}
	return fmt.Sprintf("请按 %s 构图比例生成图片。\n\n%s", ratio, prompt)
}

func externalProviderHasModel(provider ExternalImageProvider, model string) bool {
	for _, item := range provider.Models {
		if item == model {
			return true
		}
	}
	return false
}

func (s *ExternalImageService) checkLimitsLocked(owner string, now time.Time) error {
	settings := normalizeExternalImageSettings(s.settings)
	if s.running[owner] >= settings.UserConcurrentLimit {
		return ExternalImageTaskLimitError{Message: "external image concurrent task limit exceeded"}
	}
	cutoff := now.Add(-time.Minute)
	kept := s.submitTimes[owner][:0]
	for _, submitted := range s.submitTimes[owner] {
		if submitted.After(cutoff) {
			kept = append(kept, submitted)
		}
	}
	s.submitTimes[owner] = kept
	if len(kept) >= settings.UserRPMLimit {
		return ExternalImageTaskLimitError{Message: "external image RPM limit exceeded"}
	}
	return nil
}

func (s *ExternalImageService) saveReferences(owner, taskID string, uploads []ExternalImageReferenceUpload) ([]string, error) {
	if len(uploads) == 0 {
		return nil, nil
	}
	root := filepath.Join(s.config.ExternalImageReferencesDir(), safeExternalPathPart(owner), safeExternalPathPart(taskID))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(uploads))
	cleanup := func() {
		s.cleanupReferences(paths)
		_ = os.Remove(root)
		_ = os.Remove(filepath.Dir(root))
	}
	for index, upload := range uploads {
		if len(upload.Data) == 0 || len(upload.Data) > externalImageMaxReferenceBytes {
			cleanup()
			return nil, fmt.Errorf("reference image %d must be between 1 byte and 10 MB", index+1)
		}
		contentType, ext, err := validateExternalImageBytes(upload.Data, upload.ContentType)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("reference image %d: %w", index+1, err)
		}
		_ = contentType
		path := filepath.Join(root, fmt.Sprintf("%02d%s", index+1, ext))
		if err := os.WriteFile(path, upload.Data, 0o600); err != nil {
			cleanup()
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func externalReferenceNames(uploads []ExternalImageReferenceUpload) []string {
	names := make([]string, 0, len(uploads))
	for index, upload := range uploads {
		name := strings.TrimSpace(filepath.Base(upload.Name))
		if name == "" || name == "." {
			name = fmt.Sprintf("reference-%d", index+1)
		}
		names = append(names, name)
	}
	return names
}

func externalReferenceContentTypes(uploads []ExternalImageReferenceUpload) []string {
	contentTypes := make([]string, 0, len(uploads))
	for _, upload := range uploads {
		contentType, _, err := validateExternalImageBytes(upload.Data, upload.ContentType)
		if err != nil {
			contentType = strings.TrimSpace(strings.Split(upload.ContentType, ";")[0])
		}
		contentTypes = append(contentTypes, contentType)
	}
	return contentTypes
}

func validateExternalImageBytes(data []byte, claimed string) (string, string, error) {
	detected := http.DetectContentType(data)
	if claimed = strings.TrimSpace(strings.Split(claimed, ";")[0]); claimed != "" && !strings.HasPrefix(claimed, "image/") {
		return "", "", errors.New("content type is not an image")
	}
	contentType := strings.TrimSpace(strings.Split(detected, ";")[0])
	var ext string
	switch contentType {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	default:
		return "", "", errors.New("only PNG, JPEG and WebP are supported")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", "", errors.New("invalid image data")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > externalImageMaxDimension || config.Height > externalImageMaxDimension || int64(config.Width)*int64(config.Height) > externalImageMaxPixels {
		return "", "", errors.New("image dimensions exceed safety limits")
	}
	return contentType, ext, nil
}

func (s *ExternalImageService) runTask(ctx context.Context, cancel context.CancelFunc, key, owner, ownerName string, provider ExternalImageProvider, submission ExternalImageSubmission, referencePaths []string) {
	defer cancel()
	s.updateTask(key, map[string]any{"status": TaskStatusRunning, "started_at": util.NowISO()})
	release, err := s.acquireProviderSlot(ctx, provider)
	if err == nil {
		defer release()
	}
	var results []externalImageResult
	if err == nil {
		results, err = s.generate(ctx, provider, submission, referencePaths)
	}
	if err != nil {
		status := TaskStatusError
		fields := externalTaskErrorFields(err)
		if errors.Is(ctx.Err(), context.Canceled) {
			status = TaskStatusCancelled
			fields = map[string]any{"error": "任务已取消", "retryable": true}
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			fields = map[string]any{"error": "外部生图请求超时，可以稍后重试", "error_code": "timeout", "retryable": true}
		}
		s.updateTask(key, fields)
		s.finishTask(key, owner, status, nil, util.Clean(fields["error"]))
		return
	}
	items := make([]map[string]any, 0, len(results))
	for _, result := range results {
		item, saveErr := s.saveResult(owner, ownerName, provider, submission, result)
		if saveErr != nil {
			s.finishTask(key, owner, TaskStatusError, items, saveErr.Error())
			return
		}
		items = append(items, item)
		s.updateTask(key, map[string]any{"data": items})
	}
	if len(items) == 0 {
		s.finishTask(key, owner, TaskStatusError, nil, "upstream returned no image")
		return
	}
	s.finishTask(key, owner, TaskStatusSuccess, items, "")
}

func externalTaskErrorFields(err error) map[string]any {
	fields := map[string]any{"error": err.Error(), "error_detail": err.Error(), "retryable": false}
	var upstream ExternalImageUpstreamError
	if !errors.As(err, &upstream) {
		return fields
	}
	message := upstream.Message
	switch {
	case upstream.Code == "model_not_found":
		message = "上游当前没有可用于该模型的渠道，请检查渠道分组和模型映射"
	case upstream.Status == http.StatusUnauthorized || upstream.Status == http.StatusForbidden:
		message = "上游拒绝了 API Key，请检查密钥和渠道权限"
	case upstream.Status == http.StatusPaymentRequired:
		message = "上游账户余额或额度不足"
	case upstream.Status == http.StatusTooManyRequests:
		message = "上游请求过于频繁，可以稍后重试"
	case upstream.Status == http.StatusBadGateway || upstream.Status == http.StatusServiceUnavailable || upstream.Status == http.StatusGatewayTimeout:
		message = "上游生图服务暂时不可用，可以稍后重试"
	case strings.TrimSpace(message) == "":
		message = fmt.Sprintf("上游生图请求失败（HTTP %d）", upstream.Status)
	}
	if upstream.RequestID != "" {
		message += "，Request ID: " + upstream.RequestID
	}
	fields["error"] = message
	fields["error_detail"] = upstream.Detail
	fields["error_code"] = upstream.Code
	fields["upstream_status"] = upstream.Status
	fields["request_id"] = upstream.RequestID
	fields["retryable"] = upstream.Retryable()
	return fields
}

func (s *ExternalImageService) acquireProviderSlot(ctx context.Context, provider ExternalImageProvider) (func(), error) {
	s.mu.Lock()
	sem := s.providerSem[provider.ID]
	if sem == nil || cap(sem) != provider.ConcurrencyLimit {
		sem = make(chan struct{}, provider.ConcurrencyLimit)
		s.providerSem[provider.ID] = sem
	}
	s.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *ExternalImageService) updateTask(key string, updates map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[key]
	if task == nil {
		return
	}
	previous := util.CopyMap(task)
	for field, value := range updates {
		task[field] = value
	}
	task["updated_at"] = util.NowISO()
	if err := s.persistTaskLocked(key); err != nil {
		s.tasks[key] = previous
		s.logTaskPersistenceError("update", key, err)
	}
}

func (s *ExternalImageService) finishTask(key, owner, status string, items []map[string]any, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[key]
	if task == nil {
		return
	}
	if util.Clean(task["status"]) == TaskStatusCancelled {
		status = TaskStatusCancelled
		message = "任务已取消"
	}
	task["status"] = status
	if items != nil {
		task["data"] = items
	}
	task["error"] = message
	task["finished_at"] = util.NowISO()
	task["updated_at"] = util.NowISO()
	delete(s.cancels, key)
	if s.running[owner] > 0 {
		s.running[owner]--
	}
	if err := s.persistTaskLocked(key); err != nil {
		task["persistence_error"] = err.Error()
		s.logTaskPersistenceError("finish", key, err)
	}
}

func (s *ExternalImageService) logTaskPersistenceError(operation, key string, err error) {
	if err != nil && s.logger != nil {
		s.logger.Error("external image task persistence failed", "operation", operation, "task_key", key, "error", err)
	}
}

func (s *ExternalImageService) generate(ctx context.Context, provider ExternalImageProvider, submission ExternalImageSubmission, referencePaths []string) ([]externalImageResult, error) {
	switch provider.Protocol {
	case ExternalImageProtocolImages:
		return s.generateImagesAPI(ctx, provider, submission)
	case ExternalImageProtocolEdits:
		return s.generateEditsAPI(ctx, provider, submission, referencePaths)
	}
	var all []externalImageResult
	for index := 0; index < submission.N; index++ {
		items, err := s.generateChatAPI(ctx, provider, submission, referencePaths)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
	}
	return all, nil
}

func (s *ExternalImageService) generateImagesAPI(ctx context.Context, provider ExternalImageProvider, submission ExternalImageSubmission) ([]externalImageResult, error) {
	payload := map[string]any{"model": submission.Model, "prompt": externalImagePromptWithCompositionRatio(submission.Prompt, submission.AspectRatio), "n": submission.N, "response_format": "b64_json"}
	if submission.Size != "" {
		payload["size"] = submission.Size
	}
	if submission.Quality != "" {
		payload["quality"] = submission.Quality
	}
	response, err := externalPostJSON(ctx, provider, provider.BaseURL+"/images/generations", payload)
	if err != nil {
		return nil, err
	}
	var results []externalImageResult
	for _, item := range util.AsMapSlice(response["data"]) {
		result, err := s.externalResultFromItem(ctx, provider, item)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *ExternalImageService) generateEditsAPI(ctx context.Context, provider ExternalImageProvider, submission ExternalImageSubmission, referencePaths []string) ([]externalImageResult, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"model": submission.Model, "prompt": externalImagePromptWithCompositionRatio(submission.Prompt, submission.AspectRatio), "n": strconv.Itoa(submission.N),
		"size": submission.Size, "quality": submission.Quality, "response_format": "b64_json",
	} {
		if strings.TrimSpace(value) != "" {
			if err := writer.WriteField(key, value); err != nil {
				return nil, err
			}
		}
	}
	for index, path := range referencePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		contentType := mime.TypeByExtension(filepath.Ext(path))
		if contentType == "" {
			contentType = http.DetectContentType(data)
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image[]"; filename="reference-%d%s"`, index+1, filepath.Ext(path)))
		header.Set("Content-Type", contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.BaseURL+"/images/edits", &body)
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	resp, err := externalProviderAPIClient(provider).Do(req)
	if err != nil {
		return nil, err
	}
	response, err := externalReadJSONResponse(resp, provider.APIKey)
	if err != nil {
		return nil, err
	}
	var results []externalImageResult
	for _, item := range util.AsMapSlice(response["data"]) {
		result, err := s.externalResultFromItem(ctx, provider, item)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *ExternalImageService) generateChatAPI(ctx context.Context, provider ExternalImageProvider, submission ExternalImageSubmission, referencePaths []string) ([]externalImageResult, error) {
	prompt := externalImagePromptWithCompositionRatio(submission.Prompt, submission.AspectRatio)
	content := any(prompt)
	if len(referencePaths) > 0 {
		parts := []any{map[string]any{"type": "text", "text": prompt}}
		for _, path := range referencePaths {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			contentType := mime.TypeByExtension(filepath.Ext(path))
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)}})
		}
		content = parts
	}
	temperature := provider.Temperature
	if submission.Temperature != nil {
		temperature = *submission.Temperature
	}
	payload := map[string]any{
		"model": submission.Model, "messages": []any{map[string]any{"role": "user", "content": content}},
		"temperature": temperature, "stream": false,
	}
	response, err := externalPostJSON(ctx, provider, provider.BaseURL+"/chat/completions", payload)
	if err != nil {
		return nil, err
	}
	refs := externalChatImageReferences(response)
	if len(refs) == 0 {
		return nil, errors.New("Chat Completions provider returned no supported image content")
	}
	results := make([]externalImageResult, 0, len(refs))
	for _, ref := range refs {
		result, err := s.externalResultFromReference(ctx, provider, ref)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func externalPostJSON(ctx context.Context, provider ExternalImageProvider, endpoint string, payload map[string]any) (map[string]any, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := externalProviderAPIClient(provider)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return externalReadJSONResponse(resp, provider.APIKey)
}

func externalReadJSONResponse(resp *http.Response, apiKey string) (map[string]any, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, externalImageMaxDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > externalImageMaxDownloadBytes {
		return nil, errors.New("upstream response exceeds 25 MB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := externalErrorDetail(body, apiKey)
		upstreamErr := ExternalImageUpstreamError{Status: resp.StatusCode, Detail: detail}
		var payload map[string]any
		if json.Unmarshal(body, &payload) == nil {
			errorBody, _ := payload["error"].(map[string]any)
			upstreamErr.Code = util.Clean(errorBody["code"])
			upstreamErr.Message = util.Clean(errorBody["message"])
			upstreamErr.RequestID = util.Clean(errorBody["request_id"])
			if upstreamErr.RequestID == "" {
				upstreamErr.RequestID = externalRequestID(upstreamErr.Message)
			}
		}
		return nil, upstreamErr
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, errors.New("external image upstream returned invalid JSON")
	}
	return result, nil
}

func externalRequestID(message string) string {
	lower := strings.ToLower(message)
	marker := "request id:"
	index := strings.Index(lower, marker)
	if index < 0 {
		return ""
	}
	value := strings.TrimSpace(message[index+len(marker):])
	value = strings.Trim(value, "()[]{} \t\r\n")
	if end := strings.IndexAny(value, ")]} ,;\r\n\t"); end >= 0 {
		value = value[:end]
	}
	return value
}

func externalProviderAPIClient(provider ExternalImageProvider) *http.Client {
	return &http.Client{
		Timeout: time.Duration(provider.TimeoutSeconds) * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func externalErrorDetail(body []byte, secrets ...string) string {
	text := strings.TrimSpace(string(body))
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype html") {
		if strings.Contains(lower, "cloudflare") || strings.Contains(lower, "cf-ray") {
			return "upstream returned Cloudflare HTML error page"
		}
		return "upstream returned HTML error page"
	}
	if len(text) > 500 {
		text = text[:500]
	}
	return text
}

func (s *ExternalImageService) externalResultFromItem(ctx context.Context, provider ExternalImageProvider, item map[string]any) (externalImageResult, error) {
	if encoded := util.Clean(item["b64_json"]); encoded != "" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return externalImageResult{}, errors.New("invalid upstream b64_json")
		}
		contentType, _, err := validateExternalImageBytes(data, "")
		return externalImageResult{Data: data, ContentType: contentType, RevisedPrompt: util.Clean(item["revised_prompt"])}, err
	}
	if rawURL := util.Clean(item["url"]); rawURL != "" {
		result, err := s.externalResultFromReference(ctx, provider, rawURL)
		result.RevisedPrompt = util.Clean(item["revised_prompt"])
		return result, err
	}
	return externalImageResult{}, errors.New("upstream image item contains neither b64_json nor url")
}

func (s *ExternalImageService) externalResultFromReference(ctx context.Context, provider ExternalImageProvider, ref string) (externalImageResult, error) {
	if match := externalDataURLRE.FindStringSubmatch(strings.TrimSpace(ref)); len(match) == 3 {
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.ReplaceAll(match[2], "\r", ""), "\n", ""))
		if err != nil {
			return externalImageResult{}, errors.New("invalid image data URL")
		}
		contentType, _, err := validateExternalImageBytes(data, match[1])
		return externalImageResult{Data: data, ContentType: contentType}, err
	}
	return downloadExternalImage(ctx, provider, ref)
}

func externalChatImageReferences(response map[string]any) []string {
	var refs []string
	for _, choice := range util.AsMapSlice(response["choices"]) {
		message := util.StringMap(choice["message"])
		refs = append(refs, externalImageReferencesFromContent(message["content"])...)
		refs = append(refs, externalImageReferencesFromContent(message["images"])...)
	}
	return dedupeExternalStrings(refs)
}

func externalImageReferencesFromContent(value any) []string {
	var refs []string
	switch current := value.(type) {
	case string:
		for _, match := range externalDataURLRE.FindAllString(current, -1) {
			refs = append(refs, match)
		}
		for _, match := range externalMarkdownImageRE.FindAllStringSubmatch(current, -1) {
			if len(match) > 1 {
				refs = append(refs, match[1])
			}
		}
	case []any:
		for _, item := range current {
			refs = append(refs, externalImageReferencesFromContent(item)...)
		}
	case map[string]any:
		if encoded := util.Clean(current["b64_json"]); encoded != "" {
			refs = append(refs, "data:image/png;base64,"+encoded)
		}
		if raw := current["image_url"]; raw != nil {
			if obj, ok := raw.(map[string]any); ok {
				refs = append(refs, util.Clean(obj["url"]))
			} else {
				refs = append(refs, util.Clean(raw))
			}
		}
		if raw := util.Clean(current["url"]); raw != "" && strings.HasPrefix(util.Clean(current["type"]), "image") {
			refs = append(refs, raw)
		}
	}
	return refs
}

func dedupeExternalStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func downloadExternalImage(ctx context.Context, provider ExternalImageProvider, rawURL string) (externalImageResult, error) {
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
		return externalImageResult{}, errors.New("invalid upstream image URL")
	}
	if target.User != nil {
		return externalImageResult{}, errors.New("upstream image URL must not include user credentials")
	}
	if target.Scheme != "https" && !(target.Scheme == "http" && isExternalLoopback(target.Hostname())) {
		return externalImageResult{}, errors.New("upstream image URL must use HTTPS except for loopback development")
	}
	base, _ := url.Parse(provider.BaseURL)
	client := &http.Client{
		Timeout: time.Duration(provider.TimeoutSeconds) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many image download redirects")
			}
			if externalSameOrigin(base, req.URL) {
				req.Header.Set("Authorization", "Bearer "+provider.APIKey)
			} else {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if externalSameOrigin(base, target) {
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return externalImageResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return externalImageResult{}, fmt.Errorf("image download failed with status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, externalImageMaxDownloadBytes+1))
	if err != nil {
		return externalImageResult{}, err
	}
	if len(data) > externalImageMaxDownloadBytes {
		return externalImageResult{}, errors.New("downloaded image exceeds 25 MB")
	}
	claimed := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if !strings.HasPrefix(claimed, "image/") {
		return externalImageResult{}, errors.New("download response is not an image")
	}
	contentType, _, err := validateExternalImageBytes(data, claimed)
	return externalImageResult{Data: data, ContentType: contentType}, err
}

func externalSameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil || !strings.EqualFold(left.Scheme, right.Scheme) || !strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	port := func(value *url.URL) string {
		if value.Port() != "" {
			return value.Port()
		}
		if strings.EqualFold(value.Scheme, "https") {
			return "443"
		}
		if strings.EqualFold(value.Scheme, "http") {
			return "80"
		}
		return ""
	}
	return port(left) == port(right)
}

func (s *ExternalImageService) saveResult(owner, ownerName string, provider ExternalImageProvider, submission ExternalImageSubmission, result externalImageResult) (map[string]any, error) {
	if s.images == nil {
		return nil, errors.New("shared image service is required")
	}
	references := make([]GeneratedImageReference, 0, len(submission.References))
	for _, reference := range submission.References {
		references = append(references, GeneratedImageReference{
			Filename: reference.Name, ContentType: reference.ContentType, Data: append([]byte(nil), reference.Data...),
		})
	}
	return s.images.SaveGeneratedImage(result.Data, result.ContentType, owner, ownerName, GeneratedImageMetadata{
		Prompt: submission.Prompt, Model: submission.Model, Quality: submission.Quality,
		RequestedSize: submission.Size, ReferenceImages: references,
		Source: ImageSourceExternalAPI, ProviderID: provider.ID, ProviderName: provider.Name,
		Protocol: provider.Protocol, RevisedPrompt: result.RevisedPrompt,
	})
}

func (s *ExternalImageService) migrateLegacyImagesLocked() {
	if s.images == nil {
		return
	}
	dataRoot := filepath.Dir(s.config.ExternalImageReferencesDir())
	imageRoot := filepath.Join(dataRoot, "external-images")
	metadataRoot := filepath.Join(dataRoot, "external-image-metadata")
	thumbnailRoot := filepath.Join(dataRoot, "external-image-thumbnails")
	changedTasks := map[string]struct{}{}
	_ = filepath.WalkDir(metadataRoot, func(metaPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		data, err := os.ReadFile(metaPath)
		if err != nil {
			return nil
		}
		var legacy map[string]any
		if json.Unmarshal(data, &legacy) != nil {
			return nil
		}
		legacyRel, err := cleanExternalRelativePath(util.Clean(legacy["path"]))
		if err != nil {
			return nil
		}
		imageData, err := os.ReadFile(filepath.Join(imageRoot, filepath.FromSlash(legacyRel)))
		if err != nil {
			return nil
		}
		ownerID := util.Clean(legacy["owner_id"])
		ownerName := ownerID
		for _, task := range s.tasks {
			if util.Clean(task["owner_id"]) == ownerID && util.Clean(task["owner_name"]) != "" {
				ownerName = util.Clean(task["owner_name"])
				break
			}
		}
		item, err := s.images.SaveGeneratedImage(imageData, util.Clean(legacy["content_type"]), ownerID, ownerName, GeneratedImageMetadata{
			Prompt: util.Clean(legacy["prompt"]), Model: util.Clean(legacy["model"]),
			Source: ImageSourceExternalAPI, ProviderID: util.Clean(legacy["provider_id"]),
			ProviderName: util.Clean(legacy["provider_name"]), Protocol: util.Clean(legacy["protocol"]),
			RevisedPrompt: util.Clean(legacy["revised_prompt"]),
		})
		if err != nil {
			return nil
		}
		for key, task := range s.tasks {
			items := util.AsMapSlice(task["data"])
			updated := false
			for index := range items {
				if util.Clean(items[index]["path"]) == legacyRel {
					items[index] = util.CopyMap(item)
					updated = true
				}
			}
			if updated {
				task["data"] = items
				task["updated_at"] = util.NowISO()
				changedTasks[key] = struct{}{}
			}
		}
		_ = os.Remove(filepath.Join(imageRoot, filepath.FromSlash(legacyRel)))
		if thumb := util.Clean(legacy["thumbnail_path"]); thumb != "" {
			_ = os.Remove(filepath.Join(thumbnailRoot, filepath.FromSlash(thumb)))
		}
		_ = os.Remove(metaPath)
		return nil
	})
	for key := range changedTasks {
		if err := s.persistTaskLocked(key); err != nil {
			s.tasks[key]["persistence_error"] = err.Error()
			s.logTaskPersistenceError("migrate", key, err)
		}
	}
	removeEmptyExternalDirectories(metadataRoot)
	removeEmptyExternalDirectories(imageRoot)
	removeEmptyExternalDirectories(thumbnailRoot)
}

func removeEmptyExternalDirectories(root string) {
	directories := make([]string, 0)
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	})
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		_ = os.Remove(directory)
	}
}

func (s *ExternalImageService) ListTasks(identity Identity, ids []string) []map[string]any {
	items, _ := s.ListTasksPage(identity, ids, 0, externalImageMaxTasksPerOwner)
	return items
}

func (s *ExternalImageService) ListTasksPage(identity Identity, ids []string, offset, limit int) ([]map[string]any, int) {
	owner := externalOwnerID(identity)
	if offset < 0 {
		offset = 0
	}
	if limit < 1 || limit > externalImageMaxTasksPerOwner {
		limit = 50
	}
	requested := map[string]struct{}{}
	for _, id := range ids {
		requested[strings.TrimSpace(id)] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := s.pruneExpiredTasksLocked(time.Now().UTC())
	if len(removed) > 0 {
		if err := s.deleteTaskRecordsLocked(removed); err != nil {
			s.restorePrunedTasksLocked(removed)
		} else {
			s.cleanupPrunedTaskReferences(removed)
		}
	}
	items := make([]map[string]any, 0)
	for key, task := range s.tasks {
		if identity.Role != AuthRoleAdmin && util.Clean(task["owner_id"]) != owner {
			continue
		}
		if len(requested) > 0 {
			if _, ok := requested[util.Clean(task["id"])]; !ok {
				continue
			}
		}
		if util.Clean(task["persistence_error"]) != "" {
			if err := s.persistTaskLocked(key); err != nil {
				s.logTaskPersistenceError("retry", key, err)
			}
		}
		items = append(items, externalPublicTask(task))
	}
	sort.SliceStable(items, func(i, j int) bool {
		return historyTimeAfter(util.Clean(items[i]["updated_at"]), util.Clean(items[j]["updated_at"]))
	})
	total := len(items)
	if offset >= total {
		return []map[string]any{}, total
	}
	end := min(offset+limit, total)
	return items[offset:end], total
}

func (s *ExternalImageService) pruneExpiredTasksLocked(now time.Time) map[string]map[string]any {
	cutoff := now.Add(-externalImageTaskRetention)
	removed := map[string]map[string]any{}
	for key, task := range s.tasks {
		status := util.Clean(task["status"])
		if status == TaskStatusQueued || status == TaskStatusRunning {
			continue
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, util.Clean(task["updated_at"]))
		if err != nil || !updatedAt.Before(cutoff) {
			continue
		}
		removed[key] = task
		delete(s.tasks, key)
	}
	return removed
}

func (s *ExternalImageService) restorePrunedTasksLocked(removed map[string]map[string]any) {
	for key, task := range removed {
		s.tasks[key] = task
	}
}

func (s *ExternalImageService) cleanupPrunedTaskReferences(removed map[string]map[string]any) {
	for _, task := range removed {
		s.cleanupReferences(util.AsStringSlice(task["reference_paths"]))
	}
}

func (s *ExternalImageService) ReferenceFile(identity Identity, id string, index int) ([]byte, string, string, error) {
	s.mu.Lock()
	_, task := s.findTaskLocked(identity, id)
	if task == nil {
		s.mu.Unlock()
		return nil, "", "", errors.New("external image task not found")
	}
	paths := util.AsStringSlice(task["reference_paths"])
	names := util.AsStringSlice(task["reference_names"])
	contentTypes := util.AsStringSlice(task["reference_content_types"])
	if index < 0 || index >= len(paths) {
		s.mu.Unlock()
		return nil, "", "", errors.New("external image reference not found")
	}
	path := paths[index]
	name := fmt.Sprintf("reference-%d%s", index+1, filepath.Ext(path))
	if index < len(names) && strings.TrimSpace(names[index]) != "" {
		name = names[index]
	}
	contentType := mime.TypeByExtension(filepath.Ext(path))
	if index < len(contentTypes) && strings.TrimSpace(contentTypes[index]) != "" {
		contentType = contentTypes[index]
	}
	s.mu.Unlock()
	safePath, ok := externalPathWithinRoot(s.config.ExternalImageReferencesDir(), path)
	if !ok {
		return nil, "", "", errors.New("invalid external image reference path")
	}
	data, err := os.ReadFile(safePath)
	if err != nil {
		return nil, "", "", errors.New("external image reference not found")
	}
	return data, contentType, name, nil
}

func (s *ExternalImageService) RetryTask(identity Identity, id string) (map[string]any, error) {
	s.mu.Lock()
	_, task := s.findTaskLocked(identity, id)
	if task == nil {
		s.mu.Unlock()
		return nil, errors.New("external image task not found")
	}
	status := util.Clean(task["status"])
	if status == TaskStatusQueued || status == TaskStatusRunning {
		s.mu.Unlock()
		return nil, ExternalImageTaskConflictError{Message: "running external image tasks cannot be retried"}
	}
	snapshot := util.CopyMap(task)
	s.mu.Unlock()

	references, err := s.referenceUploadsFromTask(snapshot)
	if err != nil {
		return nil, err
	}
	submission := ExternalImageSubmission{
		ClientTaskID: "external_" + util.NewHex(24), RetryOf: util.Clean(snapshot["id"]),
		ProviderID: util.Clean(snapshot["provider_id"]), Model: util.Clean(snapshot["model"]),
		Prompt: util.Clean(snapshot["prompt"]), N: util.ToInt(snapshot["n"], 1),
		Size: util.Clean(snapshot["size"]), AspectRatio: util.Clean(snapshot["aspect_ratio"]), Quality: util.Clean(snapshot["quality"]), References: references,
	}
	if temperature, ok := externalOptionalFloat(snapshot["temperature"]); ok {
		submission.Temperature = &temperature
	}
	return s.Submit(identity, submission)
}

func (s *ExternalImageService) DeleteTasks(identity Identity, ids []string) ([]map[string]any, error) {
	requested := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		requested = append(requested, id)
	}
	if len(requested) == 0 {
		return nil, errors.New("at least one external image task id is required")
	}

	s.mu.Lock()
	removed := map[string]map[string]any{}
	referencePaths := make([]string, 0)
	for _, id := range requested {
		key, task := s.findTaskLocked(identity, id)
		if task == nil {
			s.mu.Unlock()
			return nil, errors.New("external image task not found")
		}
		status := util.Clean(task["status"])
		if status == TaskStatusQueued || status == TaskStatusRunning {
			s.mu.Unlock()
			return nil, ExternalImageTaskConflictError{Message: "running external image tasks must be cancelled before deletion"}
		}
		removed[key] = task
		referencePaths = append(referencePaths, util.AsStringSlice(task["reference_paths"])...)
	}
	for key := range removed {
		delete(s.tasks, key)
	}
	if err := s.deleteTaskRecordsLocked(removed); err != nil {
		for key, task := range removed {
			s.tasks[key] = task
		}
		s.mu.Unlock()
		return nil, ExternalImageTaskPersistenceError{Operation: "delete", Err: err}
	}
	s.mu.Unlock()
	s.cleanupReferences(referencePaths)
	return s.ListTasks(identity, nil), nil
}

func (s *ExternalImageService) findTaskLocked(identity Identity, id string) (string, map[string]any) {
	id = strings.TrimSpace(id)
	if identity.Role != AuthRoleAdmin {
		key := externalTaskKey(externalOwnerID(identity), id)
		return key, s.tasks[key]
	}
	for key, task := range s.tasks {
		if util.Clean(task["id"]) == id {
			return key, task
		}
	}
	return "", nil
}

func (s *ExternalImageService) referenceUploadsFromTask(task map[string]any) ([]ExternalImageReferenceUpload, error) {
	paths := util.AsStringSlice(task["reference_paths"])
	names := util.AsStringSlice(task["reference_names"])
	contentTypes := util.AsStringSlice(task["reference_content_types"])
	uploads := make([]ExternalImageReferenceUpload, 0, len(paths))
	for index, path := range paths {
		safePath, ok := externalPathWithinRoot(s.config.ExternalImageReferencesDir(), path)
		if !ok {
			return nil, errors.New("saved reference image path is invalid")
		}
		data, err := os.ReadFile(safePath)
		if err != nil {
			return nil, errors.New("saved reference image has expired; upload it again before retrying")
		}
		name := fmt.Sprintf("reference-%d%s", index+1, filepath.Ext(path))
		if index < len(names) && strings.TrimSpace(names[index]) != "" {
			name = names[index]
		}
		contentType := mime.TypeByExtension(filepath.Ext(path))
		if index < len(contentTypes) && strings.TrimSpace(contentTypes[index]) != "" {
			contentType = contentTypes[index]
		}
		uploads = append(uploads, ExternalImageReferenceUpload{Name: name, ContentType: contentType, Data: data})
	}
	return uploads, nil
}

func externalOptionalFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		parsed, err := strconv.ParseFloat(util.Clean(value), 64)
		return parsed, err == nil
	}
}

func (s *ExternalImageService) CancelTask(identity Identity, id string) (map[string]any, error) {
	owner := externalOwnerID(identity)
	s.mu.Lock()
	defer s.mu.Unlock()
	var key string
	if identity.Role == AuthRoleAdmin {
		for candidate, task := range s.tasks {
			if util.Clean(task["id"]) == strings.TrimSpace(id) {
				key = candidate
				owner = util.Clean(task["owner_id"])
				break
			}
		}
	} else {
		key = externalTaskKey(owner, id)
	}
	task := s.tasks[key]
	if task == nil {
		return nil, errors.New("external image task not found")
	}
	status := util.Clean(task["status"])
	if status == TaskStatusQueued || status == TaskStatusRunning {
		previous := util.CopyMap(task)
		previousCancel := s.cancels[key]
		task["status"] = TaskStatusCancelled
		task["error"] = "任务已取消"
		task["updated_at"] = util.NowISO()
		_ = owner
		if err := s.persistTaskLocked(key); err != nil {
			s.tasks[key] = previous
			if previousCancel != nil {
				s.cancels[key] = previousCancel
			}
			s.logTaskPersistenceError("cancel", key, err)
			return nil, ExternalImageTaskPersistenceError{Operation: "cancel", Err: err}
		}
		if previousCancel != nil {
			previousCancel()
		}
	}
	return externalPublicTask(task), nil
}

func externalPublicTask(task map[string]any) map[string]any {
	out := util.CopyMap(task)
	paths := util.AsStringSlice(task["reference_paths"])
	names := util.AsStringSlice(task["reference_names"])
	contentTypes := util.AsStringSlice(task["reference_content_types"])
	references := make([]map[string]any, 0, len(paths))
	for index, path := range paths {
		name := fmt.Sprintf("reference-%d%s", index+1, filepath.Ext(path))
		if index < len(names) && strings.TrimSpace(names[index]) != "" {
			name = names[index]
		}
		contentType := mime.TypeByExtension(filepath.Ext(path))
		if index < len(contentTypes) && strings.TrimSpace(contentTypes[index]) != "" {
			contentType = contentTypes[index]
		}
		references = append(references, map[string]any{
			"index": index, "name": name, "content_type": contentType,
			"url": "/api/external-image-tasks/" + url.PathEscape(util.Clean(task["id"])) + "/references/" + strconv.Itoa(index),
		})
	}
	if len(references) > 0 {
		out["references"] = references
	}
	delete(out, "reference_paths")
	delete(out, "reference_names")
	delete(out, "reference_content_types")
	return out
}

func externalOwnerID(identity Identity) string {
	if owner := strings.TrimSpace(identity.OwnerID); owner != "" {
		return owner
	}
	return strings.TrimSpace(identity.ID)
}

func externalCanAccess(identity Identity, owner string) bool {
	return identity.Role == AuthRoleAdmin || externalOwnerID(identity) == strings.TrimSpace(owner)
}

func externalTaskKey(owner, id string) string {
	return strings.TrimSpace(owner) + ":" + strings.TrimSpace(id)
}

func safeExternalPathPart(value string) string {
	value = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`).ReplaceAllString(strings.TrimSpace(value), "_")
	value = strings.Trim(value, ".")
	if value == "" {
		return "unknown"
	}
	return value
}

func cleanExternalRelativePath(value string) (string, error) {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "..") || filepath.IsAbs(value) {
		return "", errors.New("invalid external image path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || strings.HasPrefix(clean, "../") {
		return "", errors.New("invalid external image path")
	}
	return clean, nil
}

func (s *ExternalImageService) cleanupReferences(paths []string) {
	for _, path := range paths {
		if safePath, ok := externalPathWithinRoot(s.config.ExternalImageReferencesDir(), path); ok {
			_ = os.Remove(safePath)
		}
	}
	for _, path := range paths {
		safePath, ok := externalPathWithinRoot(s.config.ExternalImageReferencesDir(), path)
		if !ok {
			continue
		}
		dir := filepath.Dir(safePath)
		if safeDir, dirOK := externalPathWithinRoot(s.config.ExternalImageReferencesDir(), dir); dirOK {
			_ = os.Remove(safeDir)
		}
		if safeParent, parentOK := externalPathWithinRoot(s.config.ExternalImageReferencesDir(), filepath.Dir(dir)); parentOK {
			_ = os.Remove(safeParent)
		}
	}
}

func (s *ExternalImageService) cleanupOrphanedReferences() {
	root := s.config.ExternalImageReferencesDir()
	referenced := map[string]struct{}{}
	for _, task := range s.tasks {
		for _, path := range util.AsStringSlice(task["reference_paths"]) {
			if safePath, ok := externalPathWithinRoot(root, path); ok {
				referenced[filepath.Clean(safePath)] = struct{}{}
			}
		}
	}
	directories := make([]string, 0)
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		if _, ok := externalPathWithinRoot(root, path); !ok {
			return nil
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if _, ok := referenced[filepath.Clean(path)]; !ok {
			_ = os.Remove(path)
		}
		return nil
	})
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		_ = os.Remove(directory)
	}
}

func externalPathWithinRoot(root, candidate string) (string, bool) {
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", false
	}
	candidate, err = filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return candidate, true
}
