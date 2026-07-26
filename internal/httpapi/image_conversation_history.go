package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"chatgpt2api/internal/service"
	"chatgpt2api/internal/util"
)

func (a *App) handleImageConversations(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.requireIdentity(w, r, "")
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ownerID := identityScope(identity)

	switch r.Method {
	case http.MethodGet:
		document, err := a.imageHistory.List(ownerID)
		if err != nil {
			util.WriteError(w, http.StatusInternalServerError, "failed to load image conversation history")
			return
		}
		util.WriteJSON(w, http.StatusOK, document)
	case http.MethodPut:
		document, err := readImageConversationHistory(r)
		if err != nil {
			writeRequestBodyError(w, err, "invalid image conversation history")
			return
		}
		document, err = a.imageHistory.Sync(ownerID, document)
		if err != nil {
			util.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		util.WriteJSON(w, http.StatusOK, document)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func readImageConversationHistory(r *http.Request) (service.ImageConversationHistoryDocument, error) {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var document service.ImageConversationHistoryDocument
	if err := decoder.Decode(&document); err != nil {
		return service.ImageConversationHistoryDocument{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return service.ImageConversationHistoryDocument{}, errors.New("request body must contain one JSON object")
		}
		return service.ImageConversationHistoryDocument{}, err
	}
	if document.Items == nil {
		document.Items = []json.RawMessage{}
	}
	if document.Deletions == nil {
		document.Deletions = []service.HistoryDeletion{}
	}
	return document, nil
}
