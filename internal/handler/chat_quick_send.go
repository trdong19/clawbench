package handler

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"clawbench/internal/service"
)

// ServeChatQuickSend handles GET (list) and POST (create) for chat quick-send items,
// and PUT /reorder for batch reordering.
func ServeChatQuickSend(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		serveQuickSendGet(w, r)
	case http.MethodPost:
		serveQuickSendPost(w, r)
	case http.MethodPut:
		serveQuickSendPut(w, r)
	default:
		writeLocalizedErrorf(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func serveQuickSendGet(w http.ResponseWriter, r *http.Request) {
	items, err := service.GetChatQuickSend()
	if err != nil {
		slog.Error("failed to get chat quick-send items", slog.String("error", err.Error()))
		writeLocalizedErrorf(w, r, http.StatusInternalServerError, "InternalError")
		return
	}
	if items == nil {
		items = []service.ChatQuickSendItem{}
	}
	writeJSON(w, http.StatusOK, items)
}

func serveQuickSendPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label   string `json:"label"`
		Command string `json:"command"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Label = strings.TrimSpace(req.Label)
	req.Command = strings.TrimSpace(req.Command)
	if !validateQuickSendFields(w, r, req.Label, req.Command) {
		return
	}
	id, err := service.AddChatQuickSend(req.Label, req.Command)
	if err != nil {
		slog.Error("failed to add chat quick-send item", slog.String("error", err.Error()))
		writeLocalizedErrorf(w, r, http.StatusInternalServerError, "InternalError")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "label": req.Label, jsonKeyCommand: req.Command,
	})
}

func serveQuickSendPut(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/chat/quick-send")
	if strings.TrimPrefix(path, "/") == jsonKeyReorder {
		serveQuickSendReorder(w, r)
		return
	}
	writeLocalizedErrorf(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
}

func serveQuickSendReorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{jsonKeySuccess: true})
		return
	}
	if err := service.ReorderChatQuickSend(req.IDs); err != nil {
		slog.Error("failed to reorder chat quick-send items", slog.String("error", err.Error()))
		writeLocalizedErrorf(w, r, http.StatusInternalServerError, "InternalError")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{jsonKeySuccess: true})
}

// validateQuickSendFields validates label and command for quick-send items.
func validateQuickSendFields(w http.ResponseWriter, r *http.Request, label, command string) bool {
	label = strings.TrimSpace(label)
	command = strings.TrimSpace(command)
	if label == "" || command == "" {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidRequestBody")
		return false
	}
	if len(label) > 100 || len(command) > 4096 {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidRequestBody")
		return false
	}
	return true
}

// ServeChatQuickSendByID handles PUT (update) and DELETE for a single chat quick-send item.
func ServeChatQuickSendByID(w http.ResponseWriter, r *http.Request) {
	// Extract ID from path: /api/chat/quick-send/{id}
	path := strings.TrimPrefix(r.URL.Path, "/api/chat/quick-send/")
	idStr := strings.TrimSuffix(path, "/")
	// Handle sub-paths like "reorder" — those should go to ServeChatQuickSend
	if idStr == "" || idStr == jsonKeyReorder {
		ServeChatQuickSend(w, r)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidRequestBody")
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Label   string `json:"label"`
			Command string `json:"command"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		req.Label = strings.TrimSpace(req.Label)
		req.Command = strings.TrimSpace(req.Command)
		if req.Label == "" || req.Command == "" {
			writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidRequestBody")
			return
		}
		if len(req.Label) > 100 || len(req.Command) > 4096 {
			writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidRequestBody")
			return
		}
		if err := service.UpdateChatQuickSend(id, req.Label, req.Command); err != nil {
			slog.Error("failed to update chat quick-send item", slog.String("error", err.Error()))
			writeLocalizedErrorf(w, r, http.StatusInternalServerError, "InternalError")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{jsonKeySuccess: true})

	case http.MethodDelete:
		if err := service.DeleteChatQuickSend(id); err != nil {
			slog.Error("failed to delete chat quick-send item", slog.String("error", err.Error()))
			writeLocalizedErrorf(w, r, http.StatusInternalServerError, "InternalError")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{jsonKeySuccess: true})

	default:
		writeLocalizedErrorf(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}
