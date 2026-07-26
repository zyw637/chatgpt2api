package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chatgpt2api/internal/service"
	"chatgpt2api/internal/util"
)

func externalHTTPTestPNG(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	canvas.Set(0, 0, color.RGBA{R: 20, G: 86, B: 240, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, canvas); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestExternalImageHTTPAPIIsolationAndRBAC(t *testing.T) {
	pngData := externalHTTPTestPNG(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
			"b64_json": base64.StdEncoding.EncodeToString(pngData),
		}}})
	}))
	defer upstream.Close()

	app := newTestApp(t)
	defer app.Close()
	handler := app.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/external-image-providers", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous providers status = %d body = %s", res.Code, res.Body.String())
	}

	_, userKey, err := app.auth.CreateAPIKey(service.AuthRoleUser, "external-user", service.AuthOwner{})
	if err != nil {
		t.Fatal(err)
	}
	userAuthorization := "Bearer " + userKey
	req = httptest.NewRequest(http.MethodGet, "/api/admin/external-image-providers", nil)
	req.Header.Set("Authorization", userAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("user admin providers status = %d body = %s", res.Code, res.Body.String())
	}
	providerManager, providerManagerKey, err := app.auth.CreateAPIKey(service.AuthRoleUser, "external-provider-manager", service.AuthOwner{})
	if err != nil {
		t.Fatal(err)
	}
	providerManagerAuthorization := "Bearer " + providerManagerKey
	providerManagerRole, err := app.auth.CreateRole(map[string]any{
		"name":       "external provider manager",
		"menu_paths": []string{"/external-image/providers"},
		"api_permissions": []string{
			service.APIPermissionKey(http.MethodGet, "/api/admin/external-image-providers"),
			service.APIPermissionKey(http.MethodPost, "/api/admin/external-image-providers"),
			service.APIPermissionKey(http.MethodPatch, "/api/admin/external-image-providers"),
			service.APIPermissionKey(http.MethodDelete, "/api/admin/external-image-providers"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated, _ := app.auth.UpdateUser(providerManager["id"].(string), map[string]any{"role_id": providerManagerRole["id"]}); updated == nil {
		t.Fatal("assign external provider manager role returned nil")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/admin/external-image-providers", nil)
	req.Header.Set("Authorization", providerManagerAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("delegated provider manager status = %d body = %s", res.Code, res.Body.String())
	}

	providerBody := map[string]any{
		"name": "HTTP Provider", "enabled": true, "image_enabled": true, "protocol": service.ExternalImageProtocolImages,
		"base_url": upstream.URL + "/v1", "api_key": "http-secret-key",
		"models": []string{"gpt-image-2"}, "default_model": "gpt-image-2",
		"temperature": 0.7, "timeout_seconds": 30, "concurrency_limit": 2,
	}
	encodedProvider, _ := json.Marshal(providerBody)
	req = httptest.NewRequest(http.MethodPost, "/api/admin/external-image-providers", bytes.NewReader(encodedProvider))
	req.Header.Set("Authorization", providerManagerAuthorization)
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("create provider status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "http-secret-key") || strings.Contains(res.Body.String(), `"api_key"`) {
		t.Fatalf("create provider exposed API key: %s", res.Body.String())
	}
	var providerResponse map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &providerResponse); err != nil {
		t.Fatal(err)
	}
	provider, _ := providerResponse["item"].(map[string]any)
	providerID, _ := provider["id"].(string)
	if providerID == "" || provider["has_api_key"] != true {
		t.Fatalf("provider response = %#v", providerResponse)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/logs?view=all", nil)
	req.Header.Set("Authorization", adminAuthHeader(t, app))
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "http-secr") {
		t.Fatalf("audit log exposed API key material: %d %s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/external-image-providers", nil)
	req.Header.Set("Authorization", userAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), upstream.URL) {
		t.Fatalf("public providers status/body = %d %s", res.Code, res.Body.String())
	}

	taskID := "http-external-task"
	generationBody := map[string]any{
		"client_task_id": taskID, "provider_id": providerID, "model": "gpt-image-2",
		"prompt": "isolated HTTP image", "n": 1, "size": "1024x1024", "quality": "high",
	}
	encodedGeneration, _ := json.Marshal(generationBody)
	req = httptest.NewRequest(http.MethodPost, "/api/external-image-tasks/generations", bytes.NewReader(encodedGeneration))
	req.Header.Set("Authorization", userAuthorization)
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("submit external task status = %d body = %s", res.Code, res.Body.String())
	}

	var taskStatus string
	waitForHTTPTestCondition(t, func() bool {
		req = httptest.NewRequest(http.MethodGet, "/api/external-image-tasks?ids="+taskID, nil)
		req.Header.Set("Authorization", userAuthorization)
		res = httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		var body map[string]any
		_ = json.Unmarshal(res.Body.Bytes(), &body)
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			return false
		}
		item, _ := items[0].(map[string]any)
		taskStatus, _ = item["status"].(string)
		return taskStatus == service.TaskStatusSuccess || taskStatus == service.TaskStatusError
	})
	if taskStatus != service.TaskStatusSuccess {
		t.Fatalf("external task status = %q body = %s", taskStatus, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/images", nil)
	req.Header.Set("Authorization", userAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("managed images status = %d body = %s", res.Code, res.Body.String())
	}
	var imageResponse map[string]any
	_ = json.Unmarshal(res.Body.Bytes(), &imageResponse)
	imageItems, _ := imageResponse["items"].([]any)
	if len(imageItems) != 1 {
		t.Fatalf("managed images body = %#v", imageResponse)
	}
	imageItem, _ := imageItems[0].(map[string]any)
	if imageItem["source"] != service.ImageSourceExternalAPI || imageItem["provider_id"] != providerID {
		t.Fatalf("external image metadata = %#v", imageItem)
	}
	imagePath, _ := imageItem["path"].(string)
	imageURL, _ := imageItem["url"].(string)

	req = httptest.NewRequest(http.MethodGet, imageURL, nil)
	req.Header.Set("Authorization", userAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !bytes.Equal(res.Body.Bytes(), pngData) {
		t.Fatalf("external image file status = %d size = %d", res.Code, res.Body.Len())
	}

	_, otherUserKey, err := app.auth.CreateAPIKey(service.AuthRoleUser, "other-user", service.AuthOwner{})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, imageURL, nil)
	req.Header.Set("Authorization", "Bearer "+otherUserKey)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("other user image file status = %d body = %s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/creation-tasks", nil)
	req.Header.Set("Authorization", userAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), taskID) {
		t.Fatalf("external task leaked into creation tasks: %d %s", res.Code, res.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/external-images", nil)
	req.Header.Set("Authorization", userAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("removed external gallery status = %d body = %s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/external-image-tasks/"+taskID+"/retry", nil)
	req.Header.Set("Authorization", userAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("retry external task status = %d body = %s", res.Code, res.Body.String())
	}
	var retryTask map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &retryTask); err != nil {
		t.Fatal(err)
	}
	retryTaskID := util.Clean(retryTask["id"])
	if retryTaskID == "" || retryTask["retry_of"] != taskID {
		t.Fatalf("retry task = %#v", retryTask)
	}
	waitForHTTPTestCondition(t, func() bool {
		req = httptest.NewRequest(http.MethodGet, "/api/external-image-tasks?ids="+retryTaskID, nil)
		req.Header.Set("Authorization", userAuthorization)
		res = httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		var body map[string]any
		_ = json.Unmarshal(res.Body.Bytes(), &body)
		items := util.AsMapSlice(body["items"])
		return len(items) == 1 && util.Clean(items[0]["status"]) == service.TaskStatusSuccess
	})

	deleteTasksBody, _ := json.Marshal(map[string]any{"ids": []string{taskID, retryTaskID}})
	req = httptest.NewRequest(http.MethodDelete, "/api/external-image-tasks", bytes.NewReader(deleteTasksBody))
	req.Header.Set("Authorization", userAuthorization)
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("delete external tasks status = %d body = %s", res.Code, res.Body.String())
	}
	var deletedTasksBody map[string]any
	_ = json.Unmarshal(res.Body.Bytes(), &deletedTasksBody)
	if items := util.AsMapSlice(deletedTasksBody["items"]); len(items) != 0 {
		t.Fatalf("external tasks after delete = %#v", items)
	}
	req = httptest.NewRequest(http.MethodGet, imageURL, nil)
	req.Header.Set("Authorization", userAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("task deletion removed shared image status = %d body = %s", res.Code, res.Body.String())
	}

	deleteBody, _ := json.Marshal(map[string]any{"paths": []string{imagePath}})
	req = httptest.NewRequest(http.MethodDelete, "/api/images", bytes.NewReader(deleteBody))
	req.Header.Set("Authorization", adminAuthHeader(t, app))
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("delete managed image status = %d body = %s", res.Code, res.Body.String())
	}

	if !strings.Contains(imageURL, imagePath) {
		t.Fatalf("image URL = %q, want path %q", imageURL, imagePath)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/admin/external-image-providers/"+providerID, nil)
	req.Header.Set("Authorization", providerManagerAuthorization)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("delegated provider delete status = %d body = %s", res.Code, res.Body.String())
	}
}
