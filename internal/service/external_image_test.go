package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chatgpt2api/internal/storage"
	"chatgpt2api/internal/util"
)

type testExternalImageConfig struct {
	root string
}

func (c testExternalImageConfig) ImagesDir() string {
	return filepath.Join(c.root, "images")
}

func (c testExternalImageConfig) ImageThumbnailsDir() string {
	return filepath.Join(c.root, "image-thumbnails")
}

func (c testExternalImageConfig) ImageMetadataDir() string {
	return filepath.Join(c.root, "image-metadata")
}

func (c testExternalImageConfig) ExternalImageReferencesDir() string {
	return filepath.Join(c.root, "external-references")
}

func (c testExternalImageConfig) ImageRetentionDays() int { return 30 }

func (c testExternalImageConfig) ImageStorageLimitBytes() int64 { return 0 }

func newTestExternalImageService(t *testing.T, backend storage.Backend, config testExternalImageConfig) *ExternalImageService {
	t.Helper()
	return NewExternalImageService(backend, config, NewImageService(config, backend), nil)
}

func testExternalIdentity(id string) Identity {
	return Identity{ID: "credential-" + id, OwnerID: id, Name: id, Role: AuthRoleUser}
}

func TestExternalImageTaskBoundsPromptAndHistory(t *testing.T) {
	svc := newTestExternalImageService(t, newTestStorageBackend(t), testExternalImageConfig{root: t.TempDir()})
	identity := testExternalIdentity("bounded-user")
	if _, err := svc.Submit(identity, ExternalImageSubmission{
		ClientTaskID: "too-long",
		ProviderID:   "provider",
		Prompt:       strings.Repeat("x", externalImageMaxPromptRunes+1),
		N:            1,
	}); err == nil || !strings.Contains(err.Error(), "prompt cannot exceed") {
		t.Fatalf("oversized prompt error = %v", err)
	}

	now := time.Now().UTC()
	svc.mu.Lock()
	for index := 0; index < 3; index++ {
		id := fmt.Sprintf("task-%d", index)
		svc.tasks[externalTaskKey("bounded-user", id)] = map[string]any{
			"id": id, "owner_id": "bounded-user", "status": TaskStatusSuccess,
			"created_at": now.Add(time.Duration(index) * time.Minute).Format(time.RFC3339Nano),
			"updated_at": now.Add(time.Duration(index) * time.Minute).Format(time.RFC3339Nano),
		}
	}
	svc.tasks[externalTaskKey("bounded-user", "expired")] = map[string]any{
		"id": "expired", "owner_id": "bounded-user", "status": TaskStatusSuccess,
		"updated_at": now.Add(-externalImageTaskRetention - time.Hour).Format(time.RFC3339Nano),
	}
	svc.mu.Unlock()

	items, total := svc.ListTasksPage(identity, nil, 1, 1)
	if total != 3 || len(items) != 1 || util.Clean(items[0]["id"]) != "task-1" {
		t.Fatalf("paged tasks = %#v total=%d", items, total)
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	canvas.Set(0, 0, color.RGBA{R: 25, G: 86, B: 240, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, canvas); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}
	return out.Bytes()
}

func createTestExternalProvider(t *testing.T, svc *ExternalImageService, serverURL, protocol string) map[string]any {
	t.Helper()
	item, err := svc.CreateProvider(ExternalImageProvider{
		Name:             "Test Provider",
		Enabled:          true,
		ImageEnabled:     true,
		Protocol:         protocol,
		BaseURL:          serverURL + "/v1",
		APIKey:           "secret-api-key",
		Models:           []string{"gpt-image-2"},
		DefaultModel:     "gpt-image-2",
		Temperature:      0.7,
		TimeoutSeconds:   30,
		ConcurrencyLimit: 2,
	})
	if err != nil {
		t.Fatalf("CreateProvider() error = %v", err)
	}
	return item
}

func waitForExternalTask(t *testing.T, svc *ExternalImageService, identity Identity, taskID string, statuses ...string) map[string]any {
	t.Helper()
	wanted := map[string]struct{}{}
	for _, status := range statuses {
		wanted[status] = struct{}{}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items := svc.ListTasks(identity, []string{taskID})
		if len(items) == 1 {
			if _, ok := wanted[util.Clean(items[0]["status"])]; ok {
				return items[0]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach statuses %v", taskID, statuses)
	return nil
}

func TestExternalImageProviderRedactsAndPreservesAPIKey(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-image-2"},{"id":"other-model"}]}`))
	}))
	defer server.Close()

	svc := newTestExternalImageService(t, newTestStorageBackend(t), testExternalImageConfig{root: t.TempDir()})
	created := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolImages)
	if _, exists := created["api_key"]; exists {
		t.Fatal("CreateProvider() exposed api_key")
	}
	if created["has_api_key"] != true {
		t.Fatalf("has_api_key = %v, want true", created["has_api_key"])
	}
	publicItems := svc.ListProviders(false)
	if len(publicItems) != 1 {
		t.Fatalf("ListProviders(false) len = %d, want 1", len(publicItems))
	}
	if _, exists := publicItems[0]["api_key"]; exists {
		t.Fatal("public provider exposed api_key")
	}
	if _, exists := publicItems[0]["base_url"]; exists {
		t.Fatal("public provider exposed base_url")
	}
	if publicItems[0]["temperature"] != 0.7 {
		t.Fatalf("public provider temperature = %v, want 0.7", publicItems[0]["temperature"])
	}

	id := util.Clean(created["id"])
	if _, err := svc.UpdateProvider(id, map[string]any{"name": "Renamed", "api_key": ""}); err != nil {
		t.Fatalf("UpdateProvider() error = %v", err)
	}
	testResult, err := svc.TestProvider(context.Background(), id)
	if err != nil {
		t.Fatalf("TestProvider() error = %v", err)
	}
	if util.ToBool(testResult["default_model_available"]) != true || len(util.AsStringSlice(testResult["missing_models"])) != 0 || len(util.AsStringSlice(testResult["available_models"])) != 2 {
		t.Fatalf("TestProvider() result = %#v", testResult)
	}
	if authorization != "Bearer secret-api-key" {
		t.Fatalf("Authorization = %q, want preserved key", authorization)
	}
}

func TestExternalImageProviderMutationsRollBackOnPersistenceFailure(t *testing.T) {
	backend := newFailingStorageBackend(t)
	svc := newTestExternalImageService(t, backend, testExternalImageConfig{root: t.TempDir()})
	backend.failDocument = externalImageProvidersDocument
	provider := ExternalImageProvider{
		Name: "Provider", Enabled: true, ImageEnabled: true, Protocol: ExternalImageProtocolImages,
		BaseURL: "https://example.com/v1", APIKey: "secret", Models: []string{"gpt-image-2"}, DefaultModel: "gpt-image-2",
	}
	if _, err := svc.CreateProvider(provider); err == nil {
		t.Fatal("CreateProvider() succeeded when persistence failed")
	}
	if items := svc.ListProviders(true); len(items) != 0 {
		t.Fatalf("failed create changed in-memory providers: %#v", items)
	}

	backend.failDocument = ""
	created, err := svc.CreateProvider(provider)
	if err != nil {
		t.Fatalf("CreateProvider() error = %v", err)
	}
	id := util.Clean(created["id"])
	beforeSettings := svc.Settings()
	backend.failDocument = externalImageProvidersDocument
	if _, err := svc.UpdateSettings(ExternalImageSettings{UserConcurrentLimit: 3, UserRPMLimit: 30, ChatUserConcurrentLimit: 3, ChatUserRPMLimit: 30}); err == nil {
		t.Fatal("UpdateSettings() succeeded when persistence failed")
	}
	if got := svc.Settings(); got != beforeSettings {
		t.Fatalf("failed settings update changed memory: %#v", got)
	}
	if _, err := svc.UpdateProvider(id, map[string]any{"name": "Changed"}); err == nil {
		t.Fatal("UpdateProvider() succeeded when persistence failed")
	}
	if items := svc.ListProviders(true); len(items) != 1 || items[0]["name"] != "Provider" {
		t.Fatalf("failed provider update changed memory: %#v", items)
	}
	if err := svc.DeleteProvider(id); err == nil {
		t.Fatal("DeleteProvider() succeeded when persistence failed")
	}
	if items := svc.ListProviders(true); len(items) != 1 || util.Clean(items[0]["id"]) != id {
		t.Fatalf("failed provider delete changed memory: %#v", items)
	}
}

func TestExternalImageProviderLoadRejectsInvalidSchema(t *testing.T) {
	backend := newTestStorageBackend(t)
	store := backend.(storage.JSONDocumentBackend)
	if err := store.SaveJSONDocument(externalImageProvidersDocument, []any{"invalid"}); err != nil {
		t.Fatalf("SaveJSONDocument() error = %v", err)
	}
	svc := newTestExternalImageService(t, backend, testExternalImageConfig{root: t.TempDir()})
	if err := svc.InitializationError(); err == nil {
		t.Fatal("invalid provider document schema was accepted")
	}
}

func TestExternalImageLegacyTaskMigrationRejectsInvalidSchema(t *testing.T) {
	backend := newTestStorageBackend(t)
	store := backend.(storage.JSONDocumentBackend)
	if err := store.SaveJSONDocument(externalImageTasksDocument, map[string]any{"unexpected": true}); err != nil {
		t.Fatalf("SaveJSONDocument() error = %v", err)
	}
	svc := newTestExternalImageService(t, backend, testExternalImageConfig{root: t.TempDir()})
	if err := svc.InitializationError(); err == nil {
		t.Fatal("invalid legacy task document schema was accepted")
	}
	legacy, err := store.LoadJSONDocument(externalImageTasksDocument)
	if err != nil || legacy == nil {
		t.Fatalf("invalid legacy task document was removed: %#v, %v", legacy, err)
	}
	stored, err := backend.(storage.ExternalImageTaskBackend).LoadExternalImageTasks()
	if err != nil || len(stored) != 0 {
		t.Fatalf("invalid legacy task document created records: %#v, %v", stored, err)
	}
}

func TestExternalImageSubmitWaitsForSameTaskSubmission(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	config := testExternalImageConfig{root: t.TempDir()}
	svc := newTestExternalImageService(t, newTestStorageBackend(t), config)
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolChat)
	identity := testExternalIdentity("alice")
	taskID := "same-task"
	key := externalTaskKey(identity.OwnerID, taskID)
	done := make(chan struct{})
	svc.mu.Lock()
	svc.submitting[key] = done
	svc.mu.Unlock()

	result := make(chan error, 1)
	go func() {
		_, err := svc.Submit(identity, ExternalImageSubmission{
			ClientTaskID: taskID, ProviderID: util.Clean(provider["id"]), Model: "gpt-image-2", Prompt: "draw", N: 1,
			References: []ExternalImageReferenceUpload{{Name: "reference.png", ContentType: "image/png", Data: testPNG(t)}},
		})
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("Submit() returned before the owner submission completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	svc.mu.Lock()
	svc.tasks[key] = map[string]any{"id": taskID, "owner_id": identity.OwnerID, "status": TaskStatusQueued, "data": []any{}}
	delete(svc.submitting, key)
	close(done)
	svc.mu.Unlock()
	if err := <-result; err != nil {
		t.Fatalf("Submit() error after owner submission completed = %v", err)
	}
	if _, err := os.Stat(filepath.Join(config.ExternalImageReferencesDir(), identity.OwnerID, taskID)); !os.IsNotExist(err) {
		t.Fatalf("waiting submission wrote reference files: stat error = %v", err)
	}
}

func TestExternalImageCancelReturnsPersistenceErrorAndKeepsTaskActive(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
	}))
	defer server.Close()
	backend := newFailingStorageBackend(t)
	config := testExternalImageConfig{root: t.TempDir()}
	svc := newTestExternalImageService(t, backend, config)
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolChat)
	identity := testExternalIdentity("alice")
	if _, err := svc.Submit(identity, ExternalImageSubmission{
		ClientTaskID: "cancel-persist", ProviderID: util.Clean(provider["id"]), Model: "gpt-image-2", Prompt: "draw", N: 1,
	}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream task did not start")
	}
	waitForExternalTask(t, svc, identity, "cancel-persist", TaskStatusRunning)
	backend.failExternalTask = true
	if _, err := svc.CancelTask(identity, "cancel-persist"); err == nil {
		t.Fatal("CancelTask() succeeded when task persistence failed")
	} else {
		var persistence ExternalImageTaskPersistenceError
		if !errors.As(err, &persistence) {
			t.Fatalf("CancelTask() error = %v, want persistence error", err)
		}
	}
	items := svc.ListTasks(identity, []string{"cancel-persist"})
	if len(items) != 1 || util.Clean(items[0]["status"]) != TaskStatusRunning {
		t.Fatalf("task after failed cancel = %#v", items)
	}
	backend.failExternalTask = false
	if _, err := svc.CancelTask(identity, "cancel-persist"); err != nil {
		t.Fatalf("CancelTask() retry error = %v", err)
	}
	svc.Close()
	close(release)
}

func TestExternalImageSubmitRollsBackOnTaskPersistenceFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	backend := newFailingStorageBackend(t)
	svc := newTestExternalImageService(t, backend, testExternalImageConfig{root: t.TempDir()})
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolImages)
	identity := testExternalIdentity("alice")

	backend.failExternalTask = true
	_, err := svc.Submit(identity, ExternalImageSubmission{
		ClientTaskID: "create-persist", ProviderID: util.Clean(provider["id"]), Model: "gpt-image-2", Prompt: "draw", N: 1,
	})
	var persistence ExternalImageTaskPersistenceError
	if !errors.As(err, &persistence) {
		t.Fatalf("Submit() error = %v, want persistence error", err)
	}
	if items := svc.ListTasks(identity, nil); len(items) != 0 {
		t.Fatalf("failed task create changed memory: %#v", items)
	}
	backend.failExternalTask = false
	stored, err := backend.LoadExternalImageTasks()
	if err != nil || len(stored) != 0 {
		t.Fatalf("failed task create changed storage: %#v, %v", stored, err)
	}
}

func TestExternalImageFinishPersistenceFailureIsVisibleAndRetried(t *testing.T) {
	backend := newFailingStorageBackend(t)
	svc := newTestExternalImageService(t, backend, testExternalImageConfig{root: t.TempDir()})
	identity := testExternalIdentity("alice")
	key := externalTaskKey(identity.OwnerID, "finish-persist")
	now := util.NowISO()
	task := map[string]any{
		"id": "finish-persist", "owner_id": identity.OwnerID, "status": TaskStatusRunning,
		"data": []any{}, "created_at": now, "updated_at": now,
	}
	if err := backend.UpsertExternalImageTask(key, task); err != nil {
		t.Fatalf("UpsertExternalImageTask() error = %v", err)
	}
	svc.mu.Lock()
	svc.tasks[key] = util.CopyMap(task)
	svc.running[identity.OwnerID] = 1
	svc.mu.Unlock()

	backend.failExternalTask = true
	svc.finishTask(key, identity.OwnerID, TaskStatusSuccess, []map[string]any{{"path": "image.png"}}, "")
	items := svc.ListTasks(identity, []string{"finish-persist"})
	if len(items) != 1 || util.Clean(items[0]["status"]) != TaskStatusSuccess || util.Clean(items[0]["persistence_error"]) == "" {
		t.Fatalf("failed finish was not visible in memory: %#v", items)
	}

	backend.failExternalTask = false
	items = svc.ListTasks(identity, []string{"finish-persist"})
	if len(items) != 1 || util.Clean(items[0]["persistence_error"]) != "" {
		t.Fatalf("finish persistence retry did not clear error: %#v", items)
	}
	stored, err := backend.LoadExternalImageTasks()
	if err != nil || len(stored) != 1 || util.Clean(stored[key]["status"]) != TaskStatusSuccess {
		t.Fatalf("retried finish record = %#v, %v", stored, err)
	}
}

func TestExternalImageDeleteRollsBackOnTaskPersistenceFailure(t *testing.T) {
	backend := newFailingStorageBackend(t)
	svc := newTestExternalImageService(t, backend, testExternalImageConfig{root: t.TempDir()})
	identity := testExternalIdentity("alice")
	now := util.NowISO()
	for _, id := range []string{"delete-first", "delete-second"} {
		key := externalTaskKey(identity.OwnerID, id)
		task := map[string]any{"id": id, "owner_id": identity.OwnerID, "status": TaskStatusSuccess, "updated_at": now}
		if err := backend.UpsertExternalImageTask(key, task); err != nil {
			t.Fatal(err)
		}
		svc.mu.Lock()
		svc.tasks[key] = task
		svc.mu.Unlock()
	}

	backend.failExternalDelete = true
	if _, err := svc.DeleteTasks(identity, []string{"delete-first", "delete-second"}); err == nil {
		t.Fatal("DeleteTasks() succeeded when record deletion failed")
	}
	if items := svc.ListTasks(identity, nil); len(items) != 2 {
		t.Fatalf("failed task delete changed memory: %#v", items)
	}

	backend.failExternalDelete = false
	if _, err := svc.DeleteTasks(identity, []string{"delete-first", "delete-second"}); err != nil {
		t.Fatalf("DeleteTasks() retry error = %v", err)
	}
	stored, err := backend.LoadExternalImageTasks()
	if err != nil || len(stored) != 0 {
		t.Fatalf("tasks after batch delete = %#v, %v", stored, err)
	}
}

func TestExternalImageErrorDetailRedactsSecrets(t *testing.T) {
	detail := externalErrorDetail([]byte(`{"error":"invalid key secret-api-key"}`), "secret-api-key")
	if strings.Contains(detail, "secret-api-key") || !strings.Contains(detail, "[REDACTED]") {
		t.Fatalf("externalErrorDetail() = %q", detail)
	}
}

func TestExternalImageUpstreamErrorClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/images/generations" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"model_not_found","message":"No available channel (request id: req-test-123)","type":"new_api_error"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	svc := newTestExternalImageService(t, newTestStorageBackend(t), testExternalImageConfig{root: t.TempDir()})
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolImages)
	alice := testExternalIdentity("alice")
	if _, err := svc.Submit(alice, ExternalImageSubmission{
		ClientTaskID: "upstream-error", ProviderID: util.Clean(provider["id"]), Model: "gpt-image-2", Prompt: "fail", N: 1,
	}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	task := waitForExternalTask(t, svc, alice, "upstream-error", TaskStatusError)
	if task["error_code"] != "model_not_found" || task["request_id"] != "req-test-123" || task["retryable"] != true || util.ToInt(task["upstream_status"], 0) != http.StatusServiceUnavailable {
		t.Fatalf("classified error task = %#v", task)
	}
	if !strings.Contains(util.Clean(task["error"]), "没有可用于该模型的渠道") || !strings.Contains(util.Clean(task["error_detail"]), "model_not_found") {
		t.Fatalf("classified error message = %#v", task)
	}
}

func TestExternalErrorDetailSummarizesHTMLPages(t *testing.T) {
	cloudflare := []byte(`<!DOCTYPE html><html><title>zyfun.shop | 502: Bad gateway</title><body>Cloudflare Ray ID: abc</body></html>`)
	if got := externalErrorDetail(cloudflare); got != "upstream returned Cloudflare HTML error page" {
		t.Fatalf("Cloudflare detail = %q", got)
	}
	if got := externalErrorDetail([]byte(`<!doctype html><html><body>bad gateway</body></html>`)); got != "upstream returned HTML error page" {
		t.Fatalf("HTML detail = %q", got)
	}
}

func TestExternalImageProviderValidationAndUnsupportedFreeTest(t *testing.T) {
	if _, err := normalizeExternalImageBaseURL("http://example.com/v1"); err == nil {
		t.Fatal("normalizeExternalImageBaseURL() accepted non-loopback HTTP")
	}
	if _, err := normalizeExternalImageBaseURL("https://example.com"); err == nil {
		t.Fatal("normalizeExternalImageBaseURL() accepted URL without /v1")
	}

	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	svc := newTestExternalImageService(t, newTestStorageBackend(t), testExternalImageConfig{root: t.TempDir()})
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolImages)
	_, err := svc.TestProvider(context.Background(), util.Clean(provider["id"]))
	if err == nil || !strings.Contains(err.Error(), "不支持通过 /models") {
		t.Fatalf("TestProvider() error = %v, want unsupported free test message", err)
	}
}

func TestExternalImageProviderDefaultsOrderingAndLimits(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	svc := newTestExternalImageService(t, newTestStorageBackend(t), testExternalImageConfig{root: t.TempDir()})
	late, err := svc.CreateProvider(ExternalImageProvider{
		Name: "Late", Enabled: true, ImageEnabled: true, SortOrder: 20, Protocol: ExternalImageProtocolImages,
		BaseURL: server.URL + "/v1", APIKey: "late-key", Models: []string{"gpt-image-2"},
		DefaultModel: "gpt-image-2", MaxImages: 1, TimeoutSeconds: 30, ConcurrencyLimit: 1,
	})
	if err != nil {
		t.Fatalf("CreateProvider(late) error = %v", err)
	}
	early, err := svc.CreateProvider(ExternalImageProvider{
		Name: "Early", Enabled: true, ImageEnabled: true, SortOrder: 10, Protocol: ExternalImageProtocolChat,
		BaseURL: server.URL + "/v1", APIKey: "early-key", Models: []string{"gpt-image-2"},
		DefaultModel: "gpt-image-2", MaxImages: 2, MaxReferenceImages: 1,
		Temperature: 0.5, TimeoutSeconds: 30, ConcurrencyLimit: 1,
	})
	if err != nil {
		t.Fatalf("CreateProvider(early) error = %v", err)
	}
	items := svc.ListProviders(false)
	if len(items) != 2 || items[0]["name"] != "Early" || items[1]["name"] != "Late" {
		t.Fatalf("ListProviders() order = %#v", items)
	}
	if late["default_size"] != "auto" || late["default_quality"] != "auto" || util.ToInt(late["max_images"], 0) != 1 {
		t.Fatalf("late provider defaults = %#v", late)
	}
	if util.ToInt(early["max_reference_images"], 0) != 1 || early["temperature"] != 0.5 {
		t.Fatalf("early provider limits = %#v", early)
	}
	alice := testExternalIdentity("alice")
	if _, err := svc.Submit(alice, ExternalImageSubmission{
		ClientTaskID: "too-many", ProviderID: util.Clean(late["id"]), Model: "gpt-image-2", Prompt: "test", N: 2,
	}); err == nil || !strings.Contains(err.Error(), "at most 1") {
		t.Fatalf("Submit(max images) error = %v", err)
	}
	pngData := testPNG(t)
	if _, err := svc.Submit(alice, ExternalImageSubmission{
		ClientTaskID: "too-many-refs", ProviderID: util.Clean(early["id"]), Model: "gpt-image-2", Prompt: "test", N: 1,
		References: []ExternalImageReferenceUpload{
			{Name: "one.png", ContentType: "image/png", Data: pngData},
			{Name: "two.png", ContentType: "image/png", Data: pngData},
		},
	}); err == nil || !strings.Contains(err.Error(), "at most 1") {
		t.Fatalf("Submit(max references) error = %v", err)
	}
	if !externalImageRequestSizeAllowed("1920x1080") || externalImageRequestSizeAllowed("50000x50000") {
		t.Fatal("externalImageRequestSizeAllowed() did not enforce dimension limits")
	}
}

func TestExternalImageImagesProtocolRequestAndURLDownload(t *testing.T) {
	pngData := testPNG(t)
	payloads := make(chan map[string]any, 1)
	var downloadAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations":
			if got := r.Header.Get("Authorization"); got != "Bearer secret-api-key" {
				t.Errorf("generation Authorization = %q", got)
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("Decode() error = %v", err)
			}
			payloads <- payload
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"url": "http://" + r.Host + "/generated.png", "revised_prompt": "revised"}}})
		case "/generated.png":
			downloadAuth.Store(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := testExternalImageConfig{root: t.TempDir()}
	backend := newTestStorageBackend(t)
	images := NewImageService(config, backend)
	svc := NewExternalImageService(backend, config, images, nil)
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolImages)
	alice := testExternalIdentity("alice")
	_, err := svc.Submit(alice, ExternalImageSubmission{
		ClientTaskID: "images-task", ProviderID: util.Clean(provider["id"]), Model: "gpt-image-2",
		Prompt: "draw a lighthouse", N: 2, Size: "1024x1024", Quality: "high",
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	task := waitForExternalTask(t, svc, alice, "images-task", TaskStatusSuccess)
	if len(util.AsMapSlice(task["data"])) != 1 {
		t.Fatalf("task data = %#v, want one saved result", task["data"])
	}
	payload := <-payloads
	if payload["model"] != "gpt-image-2" || payload["prompt"] != "draw a lighthouse" || util.ToInt(payload["n"], 0) != 2 {
		t.Fatalf("generation payload = %#v", payload)
	}
	if payload["size"] != "1024x1024" || payload["quality"] != "high" || payload["response_format"] != "b64_json" {
		t.Fatalf("generation options = %#v", payload)
	}
	if got, _ := downloadAuth.Load().(string); got != "Bearer secret-api-key" {
		t.Fatalf("download Authorization = %q, want same-origin key", got)
	}
	items := util.AsMapSlice(images.ListImages("", "", "", ImageAccessScope{OwnerID: alice.OwnerID})["items"])
	if len(items) != 1 || items[0]["revised_prompt"] != "revised" || items[0]["source"] != ImageSourceExternalAPI {
		t.Fatalf("ListImages() = %#v", items)
	}
	if _, err := os.Stat(filepath.Join(config.ImagesDir(), filepath.FromSlash(util.Clean(items[0]["path"])))); err != nil {
		t.Fatalf("saved image stat error = %v", err)
	}
}

func TestExternalImageDownloadStripsKeyOnCrossOriginRedirect(t *testing.T) {
	pngData := testPNG(t)
	var destinationAuthorization atomic.Value
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationAuthorization.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngData)
	}))
	defer destination.Close()

	var redirectAuthorization atomic.Value
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectAuthorization.Store(r.Header.Get("Authorization"))
		http.Redirect(w, r, destination.URL+"/image.png", http.StatusFound)
	}))
	defer origin.Close()

	result, err := downloadExternalImage(context.Background(), ExternalImageProvider{
		BaseURL: origin.URL + "/v1", APIKey: "secret-api-key", TimeoutSeconds: 30,
	}, origin.URL+"/redirect")
	if err != nil {
		t.Fatalf("downloadExternalImage() error = %v", err)
	}
	if !bytes.Equal(result.Data, pngData) {
		t.Fatal("downloadExternalImage() returned unexpected data")
	}
	if got, _ := redirectAuthorization.Load().(string); got != "Bearer secret-api-key" {
		t.Fatalf("same-origin redirect request Authorization = %q", got)
	}
	if got, _ := destinationAuthorization.Load().(string); got != "" {
		t.Fatalf("cross-origin destination Authorization = %q, want empty", got)
	}
}

func TestExternalImageDownloadRejectsWrongContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(testPNG(t))
	}))
	defer server.Close()
	_, err := downloadExternalImage(context.Background(), ExternalImageProvider{
		BaseURL: server.URL + "/v1", TimeoutSeconds: 30,
	}, server.URL+"/not-an-image")
	if err == nil || !strings.Contains(err.Error(), "not an image") {
		t.Fatalf("downloadExternalImage() error = %v", err)
	}
}

func TestExternalImageChatProtocolReferencesAndAggregation(t *testing.T) {
	pngData := testPNG(t)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData)
	payloads := make(chan map[string]any, 4)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		payloads <- payload
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "![result](" + dataURL + ")"}}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}}}}}}})
	}))
	defer server.Close()

	config := testExternalImageConfig{root: t.TempDir()}
	svc := newTestExternalImageService(t, newTestStorageBackend(t), config)
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolChat)
	temperature := 1.1
	alice := testExternalIdentity("alice")
	_, err := svc.Submit(alice, ExternalImageSubmission{
		ClientTaskID: "chat-task", ProviderID: util.Clean(provider["id"]), Model: "gpt-image-2",
		Prompt: "use this reference", N: 2, AspectRatio: "16:9", Temperature: &temperature,
		References: []ExternalImageReferenceUpload{{Name: "reference.png", ContentType: "image/png", Data: pngData}},
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	task := waitForExternalTask(t, svc, alice, "chat-task", TaskStatusSuccess)
	if len(util.AsMapSlice(task["data"])) != 2 || calls.Load() != 2 {
		t.Fatalf("task data count = %d, calls = %d", len(util.AsMapSlice(task["data"])), calls.Load())
	}
	for index := 0; index < 2; index++ {
		payload := <-payloads
		if payload["stream"] != false || payload["model"] != "gpt-image-2" || payload["temperature"] != 1.1 {
			t.Fatalf("chat payload = %#v", payload)
		}
		messages := util.AsMapSlice(payload["messages"])
		if len(messages) != 1 {
			t.Fatalf("messages = %#v", payload["messages"])
		}
		parts, ok := messages[0]["content"].([]any)
		if !ok || len(parts) != 2 {
			t.Fatalf("multimodal content = %#v", messages[0]["content"])
		}
		textPart := util.StringMap(parts[0])
		if !strings.Contains(util.Clean(textPart["text"]), "16:9") || !strings.Contains(util.Clean(textPart["text"]), "use this reference") {
			t.Fatalf("composition prompt = %#v", textPart["text"])
		}
		imagePart := util.StringMap(parts[1])
		imageURL := util.StringMap(imagePart["image_url"])
		if !strings.HasPrefix(util.Clean(imageURL["url"]), "data:image/png;base64,") {
			t.Fatalf("reference image URL = %#v", imageURL["url"])
		}
	}
	references := util.AsMapSlice(task["references"])
	if len(references) != 1 || util.Clean(references[0]["name"]) != "reference.png" {
		t.Fatalf("task references = %#v", task["references"])
	}
	storedReference, contentType, name, err := svc.ReferenceFile(alice, "chat-task", 0)
	if err != nil || !bytes.Equal(storedReference, pngData) || contentType != "image/png" || name != "reference.png" {
		t.Fatalf("ReferenceFile() content_type=%q name=%q error=%v", contentType, name, err)
	}
	retried, err := svc.RetryTask(alice, "chat-task")
	if err != nil {
		t.Fatalf("RetryTask() error = %v", err)
	}
	retryID := util.Clean(retried["id"])
	retryTask := waitForExternalTask(t, svc, alice, retryID, TaskStatusSuccess)
	if retryTask["retry_of"] != "chat-task" || len(util.AsMapSlice(retryTask["references"])) != 1 {
		t.Fatalf("retry task = %#v", retryTask)
	}
	if _, err := svc.DeleteTasks(alice, []string{"chat-task", retryID}); err != nil {
		t.Fatalf("DeleteTasks() error = %v", err)
	}
	if items := svc.ListTasks(alice, nil); len(items) != 0 {
		t.Fatalf("tasks after delete = %#v", items)
	}
	if matches, _ := filepath.Glob(filepath.Join(config.ExternalImageReferencesDir(), "*")); len(matches) != 0 {
		t.Fatalf("reference directory was not cleaned after task deletion: %#v", matches)
	}
}

func TestExternalImageCompositionRatioValidation(t *testing.T) {
	for _, value := range []string{"1:1", "16:9", "2.39:1"} {
		if !externalImageCompositionRatioAllowed(value) {
			t.Fatalf("externalImageCompositionRatioAllowed(%q) = false", value)
		}
	}
	for _, value := range []string{"", "auto", "16x9", "0:1", "100:1"} {
		if externalImageCompositionRatioAllowed(value) {
			t.Fatalf("externalImageCompositionRatioAllowed(%q) = true", value)
		}
	}
}

func TestExternalImageEditsProtocolMultipartAndReferenceRequirement(t *testing.T) {
	pngData := testPNG(t)
	requestDetails := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(25 << 20); err != nil {
			t.Errorf("ParseMultipartForm() error = %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		files := r.MultipartForm.File["image[]"]
		requestDetails <- map[string]any{
			"model": r.FormValue("model"), "prompt": r.FormValue("prompt"), "n": r.FormValue("n"),
			"size": r.FormValue("size"), "quality": r.FormValue("quality"), "response_format": r.FormValue("response_format"),
			"file_count": len(files),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngData)},
			map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngData)},
		}})
	}))
	defer server.Close()

	config := testExternalImageConfig{root: t.TempDir()}
	svc := newTestExternalImageService(t, newTestStorageBackend(t), config)
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolEdits)
	alice := testExternalIdentity("alice")
	if _, err := svc.Submit(alice, ExternalImageSubmission{
		ClientTaskID: "missing-reference", ProviderID: util.Clean(provider["id"]), Model: "gpt-image-2",
		Prompt: "edit", N: 1,
	}); err == nil || !strings.Contains(err.Error(), "at least one reference") {
		t.Fatalf("Submit() missing reference error = %v", err)
	}
	_, err := svc.Submit(alice, ExternalImageSubmission{
		ClientTaskID: "edit-task", ProviderID: util.Clean(provider["id"]), Model: "gpt-image-2",
		Prompt: "edit these", N: 2, Size: "1024x1024", Quality: "high",
		References: []ExternalImageReferenceUpload{
			{Name: "one.png", ContentType: "image/png", Data: pngData},
			{Name: "two.png", ContentType: "image/png", Data: pngData},
		},
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	task := waitForExternalTask(t, svc, alice, "edit-task", TaskStatusSuccess)
	if len(util.AsMapSlice(task["data"])) != 2 || len(util.AsMapSlice(task["references"])) != 2 {
		t.Fatalf("edit task = %#v", task)
	}
	details := <-requestDetails
	if details["model"] != "gpt-image-2" || details["prompt"] != "edit these" || details["n"] != "2" || details["size"] != "1024x1024" || details["quality"] != "high" || details["response_format"] != "b64_json" || details["file_count"] != 2 {
		t.Fatalf("edit request = %#v", details)
	}
}

func TestExternalImageOwnershipAndPathValidation(t *testing.T) {
	pngData := testPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngData)}}})
	}))
	defer server.Close()

	config := testExternalImageConfig{root: t.TempDir()}
	backend := newTestStorageBackend(t)
	images := NewImageService(config, backend)
	svc := NewExternalImageService(backend, config, images, nil)
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolImages)
	alice := testExternalIdentity("alice")
	bob := testExternalIdentity("bob")
	admin := Identity{ID: "admin", OwnerID: "admin", Role: AuthRoleAdmin}
	_, err := svc.Submit(alice, ExternalImageSubmission{ClientTaskID: "owned", ProviderID: util.Clean(provider["id"]), Prompt: "private", Model: "gpt-image-2", N: 1})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitForExternalTask(t, svc, alice, "owned", TaskStatusSuccess)
	if len(svc.ListTasks(bob, nil)) != 0 || len(util.AsMapSlice(images.ListImages("", "", "", ImageAccessScope{OwnerID: bob.OwnerID})["items"])) != 0 {
		t.Fatal("bob can see alice resources")
	}
	if len(svc.ListTasks(admin, nil)) != 1 || len(util.AsMapSlice(images.ListImages("", "", "", ImageAccessScope{All: true})["items"])) != 1 {
		t.Fatal("admin cannot see all external resources")
	}
	if _, err := svc.RetryTask(bob, "owned"); err == nil {
		t.Fatal("RetryTask() allowed a different owner")
	}
	if _, err := svc.DeleteTasks(bob, []string{"owned"}); err == nil {
		t.Fatal("DeleteTasks() allowed a different owner")
	}
	imageItem := util.AsMapSlice(images.ListImages("", "", "", ImageAccessScope{OwnerID: alice.OwnerID})["items"])[0]
	path := util.Clean(imageItem["path"])
	if _, err := images.ImageFileAccess(path, ImageAccessScope{OwnerID: bob.OwnerID}); err == nil {
		t.Fatal("ImageFileAccess() allowed a different owner")
	}
	if _, err := images.ImageFileAccess("../external_image_providers.json", ImageAccessScope{OwnerID: alice.OwnerID}); err == nil {
		t.Fatal("ImageFileAccess() accepted path traversal")
	}
	if result, err := images.DeleteImages([]string{path}, ImageAccessScope{OwnerID: bob.OwnerID}); err != nil || util.ToInt(result["deleted"], -1) != 0 {
		t.Fatalf("DeleteImages(other owner) = %#v, %v", result, err)
	}
	if result, err := images.DeleteImages([]string{path}, ImageAccessScope{OwnerID: alice.OwnerID}); err != nil || util.ToInt(result["deleted"], 0) != 1 {
		t.Fatalf("DeleteImages() = %#v, %v", result, err)
	}
	outsidePath := filepath.Join(t.TempDir(), "must-remain.png")
	if err := os.WriteFile(outsidePath, pngData, 0o600); err != nil {
		t.Fatal(err)
	}
	svc.cleanupReferences([]string{outsidePath})
	if _, err := os.Stat(outsidePath); err != nil {
		t.Fatalf("cleanupReferences() touched an out-of-root path: %v", err)
	}
	if _, err := svc.DeleteTasks(admin, []string{"owned"}); err != nil {
		t.Fatalf("admin DeleteTasks() error = %v", err)
	}
}

func TestExternalImageCancelAndRestartRecovery(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestStarted <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-releaseRequest:
		}
	}))
	defer server.Close()
	defer close(releaseRequest)

	backend := newTestStorageBackend(t)
	config := testExternalImageConfig{root: t.TempDir()}
	svc := newTestExternalImageService(t, backend, config)
	provider := createTestExternalProvider(t, svc, server.URL, ExternalImageProtocolImages)
	alice := testExternalIdentity("alice")
	_, err := svc.Submit(alice, ExternalImageSubmission{ClientTaskID: "cancel", ProviderID: util.Clean(provider["id"]), Prompt: "cancel me", Model: "gpt-image-2", N: 1})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request did not start")
	}
	if _, err := svc.DeleteTasks(alice, []string{"cancel"}); err == nil {
		t.Fatal("DeleteTasks() deleted a running task")
	} else {
		var conflict ExternalImageTaskConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("DeleteTasks() error = %T %v, want conflict", err, err)
		}
	}
	if _, err := svc.CancelTask(alice, "cancel"); err != nil {
		t.Fatalf("CancelTask() error = %v", err)
	}
	waitForExternalTask(t, svc, alice, "cancel", TaskStatusCancelled)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		_, stillRunning := svc.cancels[externalTaskKey("alice", "cancel")]
		svc.mu.Unlock()
		if !stillRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	referencePath := filepath.Join(config.ExternalImageReferencesDir(), "alice", "stale", "01.png")
	if err := os.MkdirAll(filepath.Dir(referencePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(referencePath, testPNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(config.ExternalImageReferencesDir(), "orphan", "untracked.png")
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, testPNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	documentStore := backend.(storage.JSONDocumentBackend)
	if err := documentStore.SaveJSONDocument(externalImageTasksDocument, []any{map[string]any{
		"id": "stale", "owner_id": "alice", "status": TaskStatusRunning,
		"reference_paths": []string{referencePath}, "created_at": util.NowISO(), "updated_at": util.NowISO(),
	}}); err != nil {
		t.Fatal(err)
	}
	reloaded := newTestExternalImageService(t, backend, config)
	stale := waitForExternalTask(t, reloaded, alice, "stale", TaskStatusError)
	if !strings.Contains(util.Clean(stale["error"]), "服务重启") {
		t.Fatalf("restart error = %q", stale["error"])
	}
	legacyDocument, err := documentStore.LoadJSONDocument(externalImageTasksDocument)
	if err != nil || legacyDocument != nil {
		t.Fatalf("legacy task document was not removed after migration: %#v, %v", legacyDocument, err)
	}
	storedTasks, err := backend.(storage.ExternalImageTaskBackend).LoadExternalImageTasks()
	if err != nil {
		t.Fatalf("LoadExternalImageTasks() error = %v", err)
	}
	foundMigratedTask := false
	for _, storedTask := range storedTasks {
		if util.Clean(storedTask["id"]) == "stale" {
			foundMigratedTask = true
			break
		}
	}
	if !foundMigratedTask {
		t.Fatalf("migrated task record not found: %#v", storedTasks)
	}
	if _, err := os.Stat(referencePath); err != nil {
		t.Fatalf("stale task reference was not preserved: %v", err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan reference still exists, stat error = %v", err)
	}
}
