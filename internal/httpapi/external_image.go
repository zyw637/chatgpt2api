package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"chatgpt2api/internal/service"
	"chatgpt2api/internal/util"
)

const (
	externalMultipartImageBytes = 10 << 20
	externalMultipartImageCount = 4
)

func (a *App) handleExternalImageProviders(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r, "")
	if !ok {
		return
	}
	_ = identity
	if r.Method != http.MethodGet || r.URL.Path != "/api/external-image-providers" {
		http.NotFound(w, r)
		return
	}
	util.WriteJSON(w, http.StatusOK, map[string]any{"items": a.external.ListProviders(false)})
}

func (a *App) handleAdminExternalImageProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireIdentity(w, r, ""); !ok {
		return
	}
	base := "/api/admin/external-image-providers"
	parts := splitPath(r.URL.Path)
	switch {
	case r.URL.Path == base && r.Method == http.MethodGet:
		util.WriteJSON(w, http.StatusOK, map[string]any{"items": a.external.ListProviders(true), "settings": a.external.Settings()})
	case r.URL.Path == base && r.Method == http.MethodPost:
		body, err := readJSONMap(r)
		if err != nil {
			writeRequestBodyError(w, err, "invalid json body")
			return
		}
		provider, err := externalProviderFromMap(body)
		if err != nil {
			util.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		item, err := a.external.CreateProvider(provider)
		if err != nil {
			writeExternalProviderError(w, err, http.StatusBadRequest)
			return
		}
		util.WriteJSON(w, http.StatusOK, map[string]any{"item": item, "items": a.external.ListProviders(true)})
	case r.URL.Path == base+"/settings" && r.Method == http.MethodPatch:
		body, err := readJSONMap(r)
		if err != nil {
			writeRequestBodyError(w, err, "invalid json body")
			return
		}
		settings, err := a.external.UpdateSettings(service.ExternalImageSettings{
			UserConcurrentLimit:     util.ToInt(body["user_concurrent_limit"], 2),
			UserRPMLimit:            util.ToInt(body["user_rpm_limit"], 10),
			ChatUserConcurrentLimit: util.ToInt(body["chat_user_concurrent_limit"], 2),
			ChatUserRPMLimit:        util.ToInt(body["chat_user_rpm_limit"], 20),
		})
		if err != nil {
			writeExternalProviderError(w, err, http.StatusBadRequest)
			return
		}
		util.WriteJSON(w, http.StatusOK, map[string]any{"settings": settings})
	case len(parts) == 5 && parts[0] == "api" && parts[1] == "admin" && parts[2] == "external-image-providers" && parts[4] == "test" && r.Method == http.MethodPost:
		result, err := a.external.TestProvider(r.Context(), parts[3])
		if err != nil {
			util.WriteError(w, http.StatusBadGateway, err.Error())
			return
		}
		util.WriteJSON(w, http.StatusOK, map[string]any{"result": result})
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "admin" && parts[2] == "external-image-providers" && r.Method == http.MethodPatch:
		body, err := readJSONMap(r)
		if err != nil {
			writeRequestBodyError(w, err, "invalid json body")
			return
		}
		item, err := a.external.UpdateProvider(parts[3], body)
		if err != nil {
			writeExternalProviderError(w, err, http.StatusBadRequest)
			return
		}
		util.WriteJSON(w, http.StatusOK, map[string]any{"item": item, "items": a.external.ListProviders(true)})
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "admin" && parts[2] == "external-image-providers" && r.Method == http.MethodDelete:
		if err := a.external.DeleteProvider(parts[3]); err != nil {
			writeExternalProviderError(w, err, http.StatusNotFound)
			return
		}
		util.WriteJSON(w, http.StatusOK, map[string]any{"items": a.external.ListProviders(true)})
	default:
		http.NotFound(w, r)
	}
}

func writeExternalProviderError(w http.ResponseWriter, err error, fallbackStatus int) {
	var persistence service.ExternalImageProviderPersistenceError
	if errors.As(err, &persistence) {
		util.WriteError(w, http.StatusInternalServerError, "failed to persist external image provider configuration")
		return
	}
	util.WriteError(w, fallbackStatus, err.Error())
}

func externalProviderFromMap(body map[string]any) (service.ExternalImageProvider, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return service.ExternalImageProvider{}, err
	}
	var provider service.ExternalImageProvider
	if err := json.Unmarshal(data, &provider); err != nil {
		return service.ExternalImageProvider{}, err
	}
	return provider, nil
}

func (a *App) handleExternalImageTasks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	identity, ok := a.requireIdentity(w, r, "")
	if !ok {
		return
	}
	base := "/api/external-image-tasks"
	parts := splitPath(r.URL.Path)
	switch {
	case r.URL.Path == base && r.Method == http.MethodGet:
		page := max(1, util.ToInt(r.URL.Query().Get("page"), 1))
		pageSize := util.ToInt(r.URL.Query().Get("page_size"), 50)
		if pageSize < 1 || pageSize > 100 {
			util.WriteError(w, http.StatusBadRequest, "page_size must be between 1 and 100")
			return
		}
		items, total := a.external.ListTasksPage(identity, util.ParseCommaList(r.URL.Query().Get("ids")), (page-1)*pageSize, pageSize)
		util.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "page": page, "page_size": pageSize, "total": total})
	case r.URL.Path == base && r.Method == http.MethodDelete:
		body, err := readJSONMap(r)
		if err != nil {
			writeRequestBodyError(w, err, "invalid json body")
			return
		}
		items, err := a.external.DeleteTasks(identity, util.AsStringSlice(body["ids"]))
		if err != nil {
			var conflict service.ExternalImageTaskConflictError
			var persistence service.ExternalImageTaskPersistenceError
			if errors.As(err, &persistence) {
				util.WriteError(w, http.StatusInternalServerError, "failed to persist external image tasks")
			} else if errors.As(err, &conflict) {
				util.WriteError(w, http.StatusConflict, err.Error())
			} else {
				util.WriteError(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		util.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	case r.URL.Path == base+"/generations" && r.Method == http.MethodPost:
		submission, err := readExternalImageSubmission(r)
		if err != nil {
			writeRequestBodyError(w, err, err.Error())
			return
		}
		task, err := a.external.Submit(identity, submission)
		if err != nil {
			var limitErr service.ExternalImageTaskLimitError
			var persistence service.ExternalImageTaskPersistenceError
			if errors.As(err, &persistence) {
				util.WriteError(w, http.StatusInternalServerError, "failed to persist external image task")
			} else if errors.As(err, &limitErr) {
				util.WriteError(w, http.StatusTooManyRequests, err.Error())
			} else {
				util.WriteError(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		util.WriteJSON(w, http.StatusOK, task)
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "external-image-tasks" && parts[3] == "cancel" && r.Method == http.MethodPost:
		task, err := a.external.CancelTask(identity, parts[2])
		if err != nil {
			var persistence service.ExternalImageTaskPersistenceError
			if errors.As(err, &persistence) {
				util.WriteError(w, http.StatusInternalServerError, "failed to persist external image task")
			} else {
				util.WriteError(w, http.StatusNotFound, err.Error())
			}
			return
		}
		util.WriteJSON(w, http.StatusOK, task)
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "external-image-tasks" && parts[3] == "retry" && r.Method == http.MethodPost:
		task, err := a.external.RetryTask(identity, parts[2])
		if err != nil {
			var limitErr service.ExternalImageTaskLimitError
			var conflict service.ExternalImageTaskConflictError
			var persistence service.ExternalImageTaskPersistenceError
			switch {
			case errors.As(err, &persistence):
				util.WriteError(w, http.StatusInternalServerError, "failed to persist external image task")
			case errors.As(err, &limitErr):
				util.WriteError(w, http.StatusTooManyRequests, err.Error())
			case errors.As(err, &conflict):
				util.WriteError(w, http.StatusConflict, err.Error())
			default:
				util.WriteError(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		util.WriteJSON(w, http.StatusOK, task)
	case len(parts) == 5 && parts[0] == "api" && parts[1] == "external-image-tasks" && parts[3] == "references" && r.Method == http.MethodGet:
		index, err := strconv.Atoi(parts[4])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		data, contentType, name, err := a.external.ReferenceFile(identity, parts[2], index)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	case len(parts) == 3 && parts[0] == "api" && parts[1] == "external-image-tasks" && r.Method == http.MethodDelete:
		items, err := a.external.DeleteTasks(identity, []string{parts[2]})
		if err != nil {
			var conflict service.ExternalImageTaskConflictError
			var persistence service.ExternalImageTaskPersistenceError
			if errors.As(err, &persistence) {
				util.WriteError(w, http.StatusInternalServerError, "failed to persist external image tasks")
			} else if errors.As(err, &conflict) {
				util.WriteError(w, http.StatusConflict, err.Error())
			} else {
				util.WriteError(w, http.StatusNotFound, err.Error())
			}
			return
		}
		util.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	default:
		http.NotFound(w, r)
	}
}

func readExternalImageSubmission(r *http.Request) (service.ExternalImageSubmission, error) {
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		body, err := readJSONMap(r)
		if err != nil {
			return service.ExternalImageSubmission{}, fmt.Errorf("invalid json body: %w", err)
		}
		return externalSubmissionFromValues(body, nil), nil
	}
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		return service.ExternalImageSubmission{}, fmt.Errorf("invalid multipart body: %w", err)
	}
	defer r.MultipartForm.RemoveAll()
	values := map[string]any{}
	for _, key := range []string{"client_task_id", "provider_id", "model", "prompt", "n", "size", "aspect_ratio", "quality", "temperature"} {
		values[key] = r.FormValue(key)
	}
	references := make([]service.ExternalImageReferenceUpload, 0)
	for _, header := range r.MultipartForm.File["references"] {
		if len(references) >= externalMultipartImageCount {
			return service.ExternalImageSubmission{}, fmt.Errorf("at most %d reference images are allowed", externalMultipartImageCount)
		}
		upload, err := readExternalReference(header)
		if err != nil {
			return service.ExternalImageSubmission{}, err
		}
		references = append(references, upload)
	}
	return externalSubmissionFromValues(values, references), nil
}

func externalSubmissionFromValues(values map[string]any, references []service.ExternalImageReferenceUpload) service.ExternalImageSubmission {
	submission := service.ExternalImageSubmission{
		ClientTaskID: util.Clean(values["client_task_id"]), ProviderID: util.Clean(values["provider_id"]),
		Model: util.Clean(values["model"]), Prompt: util.Clean(values["prompt"]),
		N: util.ToInt(values["n"], 1), Size: util.Clean(values["size"]), AspectRatio: util.Clean(values["aspect_ratio"]), Quality: util.Clean(values["quality"]),
		References: references,
	}
	if raw := util.Clean(values["temperature"]); raw != "" {
		if value, err := strconv.ParseFloat(raw, 64); err == nil {
			submission.Temperature = &value
		}
	}
	return submission
}

func readExternalReference(header *multipart.FileHeader) (service.ExternalImageReferenceUpload, error) {
	file, err := header.Open()
	if err != nil {
		return service.ExternalImageReferenceUpload{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, externalMultipartImageBytes+1))
	if err != nil {
		return service.ExternalImageReferenceUpload{}, err
	}
	if len(data) > externalMultipartImageBytes {
		return service.ExternalImageReferenceUpload{}, fmt.Errorf("reference image %s exceeds 10 MB", header.Filename)
	}
	return service.ExternalImageReferenceUpload{Name: header.Filename, ContentType: header.Header.Get("Content-Type"), Data: data}, nil
}
