package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"clawbench/internal/ai"
	"clawbench/internal/model"
	"clawbench/internal/service"
)

const eventTypeCancelled = "cancelled"

// ssePrintf writes an SSE-formatted line to w. Write errors are ignored because
// SSE connection loss is handled by the heartbeat/check loop and context cancellation.
func ssePrintf(w http.ResponseWriter, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) //nolint:gosec // SSE output is JSON-encoded, not raw HTML
}

// AIChatStream handles SSE streaming for AI chat responses
func AIChatStream(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	projectPath, ok := requireProject(w, r)
	if !ok {
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = getSessionID(r)
	}
	if sessionID == "" {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "SessionIdRequired")
		return
	}

	// Verify the session belongs to the requesting project (ISS-180)
	// Skip ownership check if session doesn't exist in DB (not-yet-persisted or in-memory only)
	if sessionProject := service.GetSessionProjectPath(sessionID); sessionProject != "" && sessionProject != projectPath {
		writeLocalizedError(w, r, model.Forbidden(nil, "AccessDenied"))
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Check if session is running
	if !service.IsSessionRunning(sessionID) {
		errMsg := T(r, "SessionNotRunning")
		ssePrintf(w, "event: error\ndata: {\"error\":%q}\n\n", errMsg)
		flushResponse(w)
		return
	}

	// Get the stream channel
	streamCh, ok := service.GetSessionStream(sessionID)
	if !ok {
		errMsg := T(r, "SessionStreamNotFound")
		ssePrintf(w, "event: error\ndata: {\"error\":%q}\n\n", errMsg)
		flushResponse(w)
		return
	}

	aiChatStreamLoop(w, r, streamCh, sessionID)
}

// flushResponse flushes the HTTP response writer if it implements http.Flusher.
func flushResponse(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// sseWriteTerminal sends a terminal SSE event (done/cancelled/error) and flushes.
func sseWriteTerminal(w http.ResponseWriter, format string, args ...any) {
	ssePrintf(w, format, args...)
	flushResponse(w)
}

// writeSSEEvent marshals and writes a typed SSE event, then flushes.
func writeSSEEvent(w http.ResponseWriter, eventType string, payload any) {
	data, _ := json.Marshal(payload)
	ssePrintf(w, "event: %s\ndata: %s\n\n", eventType, data)
}

// handleSSEStreamEvent dispatches a single stream event to the SSE client.
// Returns true if the event is terminal (done/cancelled/error) and the caller should return.
func handleSSEStreamEvent(w http.ResponseWriter, event ai.StreamEvent) bool {
	switch event.Type {
	case "content":
		writeSSEEvent(w, "content", map[string]string{"content": event.Content})
	case "thinking":
		writeSSEEvent(w, "thinking", map[string]string{jsonKeyText: event.Content})
	case jsonKeyToolUse:
		writeToolUseEvent(w, event)
	case "tool_result":
		writeToolResultEvent(w, event)
	case jsonKeyMetadata:
		writeSSEEvent(w, "metadata", event.Meta)
	case jsonKeyDone:
		sseWriteTerminal(w, "event: done\ndata: {}\n\n")
		return true
	case eventTypeCancelled:
		writeSSEEvent(w, "cancelled", map[string]string{"reason": eventTypeCancelled})
		flushResponse(w)
		return true
	case jsonKeyError:
		writeErrorEvent(w, event)
		flushResponse(w)
		return true
	case jsonKeyWarning:
		writeWarningEvent(w, event)
	default:
		return handleSSEQueueEvents(w, event)
	}
	flushResponse(w)
	return false
}

// handleSSEQueueEvents handles queue-related SSE events. Returns true for terminal events.
func handleSSEQueueEvents(w http.ResponseWriter, event ai.StreamEvent) bool {
	switch event.Type {
	case "queue_consume":
		if event.QueueEvent != nil {
			writeSSEEvent(w, "queue_consume", map[string]any{
				jsonKeyText:  event.QueueEvent.Text,
				"filePaths":  event.QueueEvent.FilePaths,
				jsonKeyFiles: event.QueueEvent.Files,
			})
		}
	case jsonKeyQueueUpdate:
		if event.QueueEvent != nil {
			writeSSEEvent(w, "queue_update", map[string]any{
				jsonKeyQueue: event.QueueEvent.Queue,
			})
		}
	case "queue_done":
		ssePrintf(w, "event: queue_done\ndata: {}\n\n")
	case "resume_split":
		ssePrintf(w, "event: resume_split\ndata: {}\n\n")
	}
	return false
}

// writeToolUseEvent writes a tool_use SSE event.
func writeToolUseEvent(w http.ResponseWriter, event ai.StreamEvent) {
	if event.Tool == nil {
		return
	}
	var input any
	if event.Tool.Input != "" {
		_ = json.Unmarshal([]byte(event.Tool.Input), &input)
	}
	if input == nil {
		input = map[string]any{}
	}
	payload := map[string]any{
		"name":      event.Tool.Name,
		"id":        event.Tool.ID,
		"input":     input,
		jsonKeyDone: event.Tool.Done,
	}
	if event.Tool.Output != "" {
		payload["output"] = event.Tool.Output
	}
	if event.Tool.Status != "" {
		payload["status"] = event.Tool.Status
	}
	writeSSEEvent(w, "tool_use", payload)
}

// writeToolResultEvent writes a tool_result SSE event.
func writeToolResultEvent(w http.ResponseWriter, event ai.StreamEvent) {
	if event.Tool == nil {
		return
	}
	payload := map[string]any{
		"id": event.Tool.ID,
	}
	if event.Tool.Output != "" {
		payload["output"] = event.Tool.Output
	}
	if event.Tool.Status != "" {
		payload["status"] = event.Tool.Status
	}
	writeSSEEvent(w, "tool_result", payload)
}

// writeErrorEvent writes an error SSE event.
func writeErrorEvent(w http.ResponseWriter, event ai.StreamEvent) {
	payload := map[string]string{jsonKeyError: event.Error}
	if event.Reason != "" {
		payload["reason"] = event.Reason
	}
	writeSSEEvent(w, "error", payload)
}

// writeWarningEvent writes a warning SSE event.
func writeWarningEvent(w http.ResponseWriter, event ai.StreamEvent) {
	payload := map[string]string{jsonKeyText: event.Content}
	if event.Reason != "" {
		payload["reason"] = event.Reason
	}
	writeSSEEvent(w, "warning", payload)
}

// aiChatStreamLoop runs the main SSE event loop for AI chat streaming.
func aiChatStreamLoop(w http.ResponseWriter, r *http.Request, streamCh <-chan ai.StreamEvent, sessionID string) {
	heartbeatTicker := time.NewTicker(15 * time.Second)
	defer heartbeatTicker.Stop()

	checkTicker := time.NewTicker(2 * time.Second)
	defer checkTicker.Stop()

	for {
		select {
		case event, ok := <-streamCh:
			if !ok {
				sseWriteTerminal(w, "event: done\ndata: {}\n\n")
				return
			}
			if handleSSEStreamEvent(w, event) {
				return
			}

		case <-heartbeatTicker.C:
			ssePrintf(w, ": heartbeat %d\n\n", time.Now().UnixMilli())
			flushResponse(w)

		case <-checkTicker.C:
			if !service.IsSessionRunning(sessionID) {
				ssePrintf(w, "event: cancelled\ndata: {\"reason\":\"cancelled\"}\n\n")
				flushResponse(w)
				return
			}

		case <-r.Context().Done():
			service.SetCancelReason(sessionID, "disconnect")
			slog.Info(
				"sse client disconnected, ai session continues",
				slog.String("session_id", sessionID),
			)
			return
		}
	}
}
