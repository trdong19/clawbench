package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"clawbench/internal/ai"
	"clawbench/internal/model"
	"clawbench/internal/platform"
	"clawbench/internal/service"

	"github.com/google/uuid"
)

const maxChatBodySize = 10 << 20 // 10MB

const (
	toolNameAskUserQuestion = "AskUserQuestion"
	jsonKeyAgentID          = "agentId"
	jsonKeyBackend          = "backend"
	jsonKeyBlocks           = "blocks"
	jsonKeyMessages         = "messages"
	jsonKeyDeleted          = "deleted"
	jsonKeyError            = "error"
	jsonKeyDone             = "done"
	jsonKeyMetadata         = "metadata"
	jsonKeyQueue            = "queue"
	jsonKeyQueueUpdate      = "queue_update"
	jsonKeyCodebuddy        = "codebuddy"
	jsonKeyCodex            = "codex"
	jsonKeyDeepSeek         = "deepseek"
	jsonKeyHasMore          = "hasMore"
	jsonKeyHasUncommitted   = "hasUncommitted"
	jsonKeyIsGit            = "isGit"
	jsonKeyCommits          = "commits"
	jsonKeyFiles            = "files"
	jsonKeyErrorDetail      = "errorDetail"
	jsonKeyNotGitRepo       = "not_git_repo"
	jsonKeyInvalidRequest   = "invalid_request"
	jsonKeyDeleteFailed     = "delete_failed"
	jsonKeyMessage          = "message"
	jsonKeyEnabled          = "enabled"
	jsonKeyCommand          = "command"
	jsonKeyPath             = "path"
	jsonKeyReorder          = "reorder"
	jsonKeyFile             = "file"
	jsonKeyImage            = "image"
	jsonKeyDir              = "dir"
	jsonKeyNone             = "none"
	jsonKeyKokoro           = "kokoro"
	jsonKeyRunning          = "running"
	jsonKeyText             = "text"
	jsonKeyUser             = "user"
	jsonKeyOpenCode         = "opencode"
	jsonKeyWarning          = "warning"
	jsonKeyToolUse          = "tool_use"
	jsonKeySessionID        = "sessionId"
	jsonKeySuccess          = "success"
	jsonKeyResults          = "results"
	jsonKeyStatus           = "status"
	jsonKeyTotal            = "total"
	jsonKeyMossNano         = "moss-nano"
	jsonKeyResult           = "result"
	jsonKeyPiper            = "piper"
	timeoutMsg30Min         = "AI response timed out (30 min)"
	roleAssistant           = "assistant"
)

// ServeAISession handles DELETE for Claude CLI internal session files.
func ServeAISession(w http.ResponseWriter, r *http.Request) {
	projectPath, ok := requireProject(w, r)
	if !ok {
		return
	}

	if !requireMethod(w, r, http.MethodDelete) {
		return
	}

	// Get Claude session directory using cross-platform path mangling
	sessionDir := platform.ClaudeProjectDir(projectPath)

	// Delete all .jsonl session files
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		// Session dir doesn't exist — nothing to delete, treat as success
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, jsonKeyDeleted: 0})
		return
	}

	deleted := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			if err := os.Remove(filepath.Join(sessionDir, entry.Name())); err == nil {
				deleted++
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, jsonKeyDeleted: deleted})
}

// AIChat handles GET (status/history) and POST (send message) for AI chat.
func AIChat(w http.ResponseWriter, r *http.Request) {
	projectPath, ok := requireProject(w, r)
	if !ok {
		return
	}

	if r.Method == http.MethodGet {
		aiChatGet(w, r, projectPath)
		return
	}

	if r.Method != http.MethodPost {
		writeLocalizedErrorf(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
		return
	}

	aiChatPost(w, r, projectPath)
}

// aiChatGet handles the GET path for AIChat: returns chat history and running status.
func aiChatGet(w http.ResponseWriter, r *http.Request, projectPath string) {
	// Check if a specific session is requested
	requestedSessionID := r.URL.Query().Get("session_id")

	var sessionID string
	var sessionBackend string
	var ok bool

	if requestedSessionID != "" {
		sessionID = requestedSessionID
		sessionBackend = service.GetSessionBackend(sessionID)
		if sessionBackend == "" {
			writeLocalizedErrorf(w, r, http.StatusNotFound, "SessionNotFound")
			return
		}
		if sessionProject := service.GetSessionProjectPath(sessionID); sessionProject != "" && sessionProject != projectPath {
			writeLocalizedError(w, r, model.Forbidden(nil, "AccessDenied"))
			return
		}
	} else {
		sessionID, sessionBackend, ok = resolveOrCreateSession(projectPath, r, w)
		if !ok {
			return
		}
	}

	setSessionID(w, sessionID)
	service.UpdateLastRead(sessionID)

	limit, beforeID := parsePaginationParams(r, projectPath, sessionBackend, sessionID)

	totalCount := service.GetChatMessageCount(sessionID)
	messages, err := service.GetChatHistoryPaged(projectPath, sessionBackend, sessionID, limit, beforeID)
	sessionInfo, _ := service.GetSessionInfo(sessionID)
	sessionTitle, sessionInfoBackend, sessionAgentID, sessionModelID, sessionThinkingEffort := extractSessionInfo(sessionInfo)
	if sessionInfoBackend != "" {
		sessionBackend = sessionInfoBackend
	}
	running := service.IsSessionRunning(sessionID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{jsonKeyMessages: []any{}, jsonKeyRunning: running, jsonKeySessionID: sessionID, "sessionTitle": sessionTitle, jsonKeyBackend: sessionBackend, jsonKeyAgentID: sessionAgentID, "modelId": sessionModelID, "thinkingEffort": sessionThinkingEffort, "total": totalCount})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{jsonKeyMessages: messages, jsonKeyRunning: running, jsonKeySessionID: sessionID, "sessionTitle": sessionTitle, jsonKeyBackend: sessionBackend, jsonKeyAgentID: sessionAgentID, "modelId": sessionModelID, "thinkingEffort": sessionThinkingEffort, "total": totalCount})
}

// resolveOrCreateSession finds the latest session or creates a new one.
func resolveOrCreateSession(projectPath string, r *http.Request, w http.ResponseWriter) (sessionID, sessionBackend string, ok bool) {
	latestID, latestBackend, err := service.GetLatestSessionID(projectPath)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			model.WriteError(w, model.Internal(fmt.Errorf("failed to find latest session")))
			return "", "", false
		}
		agentID := model.GetDefaultAgentID()
		sessionBackend2, _, _, _, agentOk := resolveAgentConfig(agentID)
		if !agentOk {
			writeLocalizedErrorf(w, r, http.StatusServiceUnavailable, "NoAgentsAvailable")
			return "", "", false
		}
		sessionID, err = service.CreateSession(projectPath, sessionBackend2, T(r, "NewSession"), agentID, "", "default", "chat")
		if err != nil {
			model.WriteError(w, model.Internal(fmt.Errorf("failed to create session")))
			return "", "", false
		}
		return sessionID, sessionBackend2, true
	}
	return latestID, latestBackend, true
}

// parsePaginationParams parses limit and before_id/before pagination params from the request.
func parsePaginationParams(r *http.Request, projectPath, sessionBackend, sessionID string) (limit, beforeID int) {
	limit = 0
	beforeID = 0
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}
	if bid := r.URL.Query().Get("before_id"); bid != "" {
		if id, err := strconv.Atoi(bid); err == nil && id > 0 {
			beforeID = id
		}
	}
	if beforeID == 0 {
		if bt := r.URL.Query().Get("before"); bt != "" {
			if id, err := service.GetMessageIDBeforeTime(projectPath, sessionBackend, sessionID, bt); err == nil && id > 0 {
				beforeID = id
			}
		}
	}
	if limit == 0 {
		limit = model.ChatInitialMessages
	}
	if limit > 100 {
		limit = 100
	}
	return limit, beforeID
}

// extractSessionInfo returns session metadata fields from a SessionInfo struct.
func extractSessionInfo(sessionInfo *service.SessionInfo) (title, backend, agentID, modelID, thinkingEffort string) {
	if sessionInfo != nil {
		return sessionInfo.Title, sessionInfo.Backend, sessionInfo.AgentID, sessionInfo.Model, sessionInfo.ThinkingEffort
	}
	return "", "", "", "", ""
}

// aiChatPost handles the POST path for AIChat: sends a message to the AI.
func aiChatPost(w http.ResponseWriter, r *http.Request, projectPath string) {
	sessionID, backendName, ok := resolveChatSession(w, r, projectPath)
	if !ok {
		return
	}

	req, ok := decodeChatRequest(w, r)
	if !ok {
		return
	}

	basePath, _ := filepath.Abs(projectPath)
	fileDir := basePath

	validatedFilePaths, validatedDirPaths, ok := validateFilePaths(w, r, basePath, req.FilePaths)
	if !ok {
		return
	}
	fileAbsPaths, ok := validateFileList(w, r, basePath, req.Files)
	if !ok {
		return
	}

	prompt := buildPrompt(req.Message, validatedFilePaths, validatedDirPaths, fileAbsPaths)
	effectiveAgentID := req.AgentID
	if effectiveAgentID == "" {
		effectiveAgentID = model.GetDefaultAgentID()
	}
	persistSessionPrefs(sessionID, req.ModelID, req.ThinkingEffort)

	if !service.TrySetSessionRunning(sessionID) {
		handleEnqueueMessage(w, r, projectPath, backendName, sessionID, req.Message, req.Files)
		return
	}

	if _, err := service.AddChatMessage(projectPath, backendName, sessionID, "user", req.Message, req.Files, false, T(r, "FileMessage")); err != nil {
		service.SetSessionRunning(sessionID, false)
		model.WriteError(w, model.Internal(fmt.Errorf("failed to save message")))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"started": true, jsonKeySessionID: sessionID})

	streamCh := service.RegisterSessionStream(sessionID)
	startAIGoroutine(r, streamCh, projectPath, sessionID, backendName, effectiveAgentID, prompt, req.ModelID, req.ThinkingEffort, fileDir)
}

// resolveChatSession resolves or creates a session and returns its ID and backend name.
func resolveChatSession(w http.ResponseWriter, r *http.Request, projectPath string) (sessionID, backendName string, ok bool) {
	sessionID = getSessionID(r)
	if sessionID == "" {
		sessionID, ok = autoCreateSession(w, r, projectPath)
		if !ok {
			return "", "", false
		}
	}
	backendName = service.GetSessionBackend(sessionID)
	if backendName == "" {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "SessionBackendNotFound")
		return "", "", false
	}
	if sessionProject := service.GetSessionProjectPath(sessionID); sessionProject != "" && sessionProject != projectPath {
		writeLocalizedError(w, r, model.Forbidden(nil, "AccessDenied"))
		return "", "", false
	}
	return sessionID, backendName, true
}

// chatPostRequest is the request body for POST /api/ai/chat.
type chatPostRequest struct {
	Message        string   `json:"message"`
	FilePaths      []string `json:"filePaths"`
	Files          []string `json:"files"`
	AgentID        string   `json:"agentId"`
	ModelID        string   `json:"modelId"`
	ThinkingEffort string   `json:"thinkingEffort"`
}

// decodeChatRequest decodes and validates the chat POST request body.
func decodeChatRequest(w http.ResponseWriter, r *http.Request) (*chatPostRequest, bool) {
	var req chatPostRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidRequest")
		return nil, false
	}
	if req.Message == "" && len(req.Files) == 0 && len(req.FilePaths) == 0 {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "MessageOrFilesRequired")
		return nil, false
	}
	return &req, true
}

// persistSessionPrefs saves model and thinking effort preferences to the session.
func persistSessionPrefs(sessionID, modelID, thinkingEffort string) {
	if modelID != "" {
		_ = service.UpdateSessionModel(sessionID, modelID)
	}
	if thinkingEffort != "" {
		_ = service.UpdateSessionThinkingEffort(sessionID, thinkingEffort)
	}
}

// autoCreateSession creates a new session when none exists, returning the session ID.
func autoCreateSession(w http.ResponseWriter, r *http.Request, projectPath string) (string, bool) {
	if model.SessionMaxCount > 0 {
		if count, cerr := service.GetSessionCount(projectPath); cerr == nil && count >= model.SessionMaxCount {
			writeLocalizedErrorf(w, r, http.StatusConflict, "SessionLimitReached", map[string]any{"MaxCount": model.SessionMaxCount})
			return "", false
		}
	}
	agentID := model.GetDefaultAgentID()
	sessionBackend, _, _, _, ok := resolveAgentConfig(agentID)
	if !ok {
		writeLocalizedErrorf(w, r, http.StatusServiceUnavailable, "NoAgentsAvailable")
		return "", false
	}
	sessionID, err := service.CreateSession(projectPath, sessionBackend, T(r, "NewSession"), agentID, "", "default", "chat")
	if err != nil {
		model.WriteError(w, model.Internal(fmt.Errorf("failed to create session")))
		return "", false
	}
	setSessionID(w, sessionID)
	return sessionID, true
}

// validateFilePaths validates and resolves file path attachments.
func validateFilePaths(w http.ResponseWriter, r *http.Request, basePath string, filePaths []string) (validatedFiles, validatedDirs []string, ok bool) {
	validatedFilePaths := make([]string, 0, len(filePaths))
	validatedDirPaths := make([]string, 0, len(filePaths))
	for _, fp := range filePaths {
		fAbsPath, ok := validateAndResolvePath(w, r, basePath, fp)
		if !ok {
			return nil, nil, false
		}
		info, err := os.Stat(fAbsPath)
		if err != nil {
			writeLocalizedErrorf(w, r, http.StatusNotFound, "FileNotFound", map[string]any{"Path": fp})
			return nil, nil, false
		}
		if info.IsDir() {
			validatedDirPaths = append(validatedDirPaths, fAbsPath)
		} else {
			validatedFilePaths = append(validatedFilePaths, fAbsPath)
		}
	}
	return validatedFilePaths, validatedDirPaths, true
}

// validateFileList validates and resolves a list of file paths.
func validateFileList(w http.ResponseWriter, r *http.Request, basePath string, files []string) ([]string, bool) {
	fileAbsPaths := make([]string, 0, len(files))
	for _, fPath := range files {
		fAbsPath, ok := validateAndResolvePath(w, r, basePath, fPath)
		if !ok {
			return nil, false
		}
		if _, err := os.Stat(fAbsPath); err != nil {
			writeLocalizedErrorf(w, r, http.StatusNotFound, "FileNotFound", map[string]any{"Path": fPath})
			return nil, false
		}
		fileAbsPaths = append(fileAbsPaths, fAbsPath)
	}
	return fileAbsPaths, true
}

// buildPrompt assembles the prompt from message and file/directory paths.
func buildPrompt(message string, filePaths, dirPaths, fileAbsPaths []string) string {
	prompt := message
	if len(filePaths) > 0 {
		prompt = fmt.Sprintf("[Current file: %s]\n%s", strings.Join(filePaths, ", "), message)
	}
	if len(dirPaths) > 0 {
		prompt = fmt.Sprintf("[Current directory: %s]\n%s", strings.Join(dirPaths, ", "), prompt)
	}
	if len(fileAbsPaths) > 0 {
		prompt = fmt.Sprintf("[User uploaded %d file(s): %s]\n%s", len(fileAbsPaths), strings.Join(fileAbsPaths, ", "), prompt)
	}
	return prompt
}

// handleEnqueueMessage handles the case when a session is already running — enqueues the message.
func handleEnqueueMessage(w http.ResponseWriter, r *http.Request, projectPath, backendName, sessionID, message string, allFiles []string) {
	qMsg := model.QueuedMessage{
		Text:      message,
		FilePaths: allFiles,
		Files:     allFiles,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	queueState := service.EnqueueMessage(sessionID, qMsg)
	_, _ = service.AddChatMessage(projectPath, backendName, sessionID, "user", message, allFiles, false, T(r, "FileMessage"))
	service.SendSessionEvent(sessionID, ai.StreamEvent{
		Type:       jsonKeyQueueUpdate,
		QueueEvent: &ai.QueueEventData{Queue: queueState},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		jsonKeyRunning: true,
		"queued":       true,
		jsonKeyQueue:   queueState,
	})
}

// startAIGoroutine launches the background goroutine that runs the AI backend.
func startAIGoroutine(r *http.Request, streamCh chan ai.StreamEvent, projectPath, sessionID, backendName, agentID, prompt, modelID, thinkingEffort, fileDir string) {
	slog.Info("about to start ai goroutine", slog.String("project", projectPath))

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error(
					"AI goroutine panicked",
					slog.String("session", sessionID),
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())),
				)
				service.SetSessionRunning(sessionID, false)
				service.UnregisterSessionCancel(sessionID)
				service.SendSessionEvent(sessionID, ai.StreamEvent{Type: jsonKeyError, Error: "AI internal error, please retry", Reason: ai.ReasonPanic})
				service.UnregisterSessionStream(sessionID)
				errMsg := "AI internal error, please retry"
				errContent, _ := json.Marshal(map[string]any{jsonKeyBlocks: []any{map[string]string{"type": jsonKeyError, jsonKeyText: errMsg, "reason": ai.ReasonPanic}}})
				_ = service.FinalizeStreamingMessage(projectPath, backendName, sessionID, string(errContent))
			}
		}()
		slog.Info("ai goroutine started", slog.String("project", projectPath))
		defer service.SetSessionRunning(sessionID, false)
		defer service.UnregisterSessionStream(sessionID)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		service.RegisterSessionCancel(sessionID, cancel)
		defer service.UnregisterSessionCancel(sessionID)

		firstChatReq := buildChatRequest(prompt, sessionID, projectPath, backendName, agentID, modelID, thinkingEffort, fileDir)
		result := executeStreamRun(ctx, r, streamCh, projectPath, sessionID, backendName, agentID, firstChatReq, fileDir)

		drainQueueLoop(ctx, r, streamCh, projectPath, sessionID, backendName, agentID, result, fileDir)
	}()
}

// drainQueueLoop processes queued messages after an initial AI response completes.
func drainQueueLoop(ctx context.Context, r *http.Request, streamCh chan ai.StreamEvent, projectPath, sessionID, backendName, agentID string, result streamRunResult, fileDir string) {
	for {
		if result.cancelReason == jsonKeyUser {
			service.ClearQueue(sessionID)
			sendFinalEvent(streamCh, ai.StreamEvent{Type: "cancelled"})
			return
		}
		if result.err != "" {
			sendFinalEvent(streamCh, ai.StreamEvent{Type: jsonKeyError, Error: result.err})
			return
		}
		if result.empty {
			sendFinalEvent(streamCh, ai.StreamEvent{Type: jsonKeyError, Error: "AI returned no content", Reason: ai.ReasonEmpty})
			return
		}
		if result.cancelReason != "" {
			sendFinalEvent(streamCh, ai.StreamEvent{Type: "cancelled"})
			return
		}

		qMsg, ok := service.DequeueMessage(sessionID)
		if !ok {
			time.Sleep(50 * time.Millisecond)
			qMsg, ok = service.DequeueMessage(sessionID)
		}
		if !ok {
			sendFinalEvent(streamCh, ai.StreamEvent{Type: jsonKeyDone})
			return
		}

		slog.Info("draining queued message", slog.String("session", sessionID), slog.String("text", qMsg.Text))
		sendEvent(ctx, streamCh, ai.StreamEvent{Type: "queue_done"})
		sendEvent(ctx, streamCh, ai.StreamEvent{
			Type:       "queue_consume",
			QueueEvent: &ai.QueueEventData{Text: qMsg.Text, FilePaths: qMsg.FilePaths, Files: qMsg.Files},
		})
		_, _ = service.AddChatMessage(projectPath, backendName, sessionID, "user", qMsg.Text, qMsg.Files, false, T(r, "FileMessage"))
		remainingQueue := service.GetQueue(sessionID)
		sendEvent(ctx, streamCh, ai.StreamEvent{
			Type:       jsonKeyQueueUpdate,
			QueueEvent: &ai.QueueEventData{Queue: remainingQueue},
		})

		nextChatReq := buildChatRequestFromQueue(qMsg, sessionID, projectPath, backendName, agentID, fileDir)
		result = executeStreamRun(ctx, r, streamCh, projectPath, sessionID, backendName, agentID, nextChatReq, fileDir)
	}
}

// streamRunResult captures the outcome of a single AI stream execution.
type streamRunResult struct {
	cancelReason string // "", "user"
	err          string // error message if execution failed
	empty        bool   // true if AI returned no content
}

// executeStreamRun runs one AI backend execution from start to finish.
// It handles event accumulation, incremental DB persistence, resume_split,
// and finalizes the streaming message in the DB.
// It does NOT send a terminal SSE event — the caller decides what to send.
func executeStreamRun(
	ctx context.Context,
	r *http.Request,
	streamCh chan<- ai.StreamEvent,
	projectPath, sessionID, backendName, agentID string,
	chatReq ai.ChatRequest,
	_ string,
) streamRunResult {
	backend, err := ai.NewBackend(backendName)
	if err != nil {
		slog.Error("failed to create backend", slog.String("backend", backendName), slog.String("err", err.Error()))
		errMsg := T(r, "BackendCreateFailed", map[string]any{"Error": err.Error()})
		if !sendEvent(ctx, streamCh, ai.StreamEvent{Type: jsonKeyError, Error: errMsg}) {
			return streamRunResult{err: errMsg}
		}
		_, _ = service.AddChatMessage(projectPath, backendName, sessionID, roleAssistant, errMsg, nil, false, "")
		return streamRunResult{err: errMsg}
	}

	eventCh, err := backend.ExecuteStream(ctx, chatReq)
	if err != nil {
		slog.Error("failed to start stream", slog.String("err", err.Error()))
		errMsg := T(r, "StreamStartFailed", map[string]any{"Error": err.Error()})
		if !sendEvent(ctx, streamCh, ai.StreamEvent{Type: jsonKeyError, Error: errMsg}) {
			return streamRunResult{err: errMsg}
		}
		_, _ = service.AddChatMessage(projectPath, backendName, sessionID, roleAssistant, errMsg, nil, false, "")
		return streamRunResult{err: errMsg}
	}

	wallStart := time.Now()

	// Create streaming placeholder message in DB
	emptyContent, _ := json.Marshal(map[string]any{jsonKeyBlocks: []any{}})
	_, _ = service.AddChatMessage(projectPath, backendName, sessionID, roleAssistant, string(emptyContent), nil, true, "")

	return executeStreamEventLoop(ctx, streamCh, projectPath, backendName, sessionID, agentID, eventCh, wallStart)
}

// streamRunState holds mutable state for the stream event loop.
type streamRunState struct {
	blocks           []model.ContentBlock
	responseMetadata *ai.Metadata
	rawOutput        string
	eventCount       int
}

// executeStreamEventLoop processes the AI event channel until the stream ends or context is canceled.
// processStreamEvent handles a single event from the stream channel, returning
// the finalization result if the stream should end, or nil to continue.
func processStreamEvent(
	ctx context.Context,
	streamCh chan<- ai.StreamEvent,
	projectPath, backendName, sessionID, agentID string,
	state *streamRunState,
	eventCh <-chan ai.StreamEvent,
	wallStart time.Time,
	serializeBlocks func() string,
	event ai.StreamEvent,
) *streamRunResult {
	if handleStreamEvent(backendName, sessionID, state, event) {
		return nil
	}
	// Forward to SSE channel
	if !sendEvent(ctx, streamCh, event) {
		result := finalizeStreamRun(ctx, streamCh, projectPath, backendName, sessionID, agentID,
			ai.ChatRequest{}, state.blocks, state.responseMetadata, state.rawOutput, eventCh, wallStart)
		return &result
	}

	ai.AccumulateBlock(&state.blocks, event)

	if event.Type == "resume_split" {
		handleResumeSplit(projectPath, backendName, sessionID, state, serializeBlocks)
		return nil
	}

	if event.Type == jsonKeyMetadata && event.Meta != nil {
		state.responseMetadata = event.Meta
		captureExternalSessionID(backendName, sessionID, event.Meta.SessionID)
	}
	state.eventCount++
	if state.eventCount%5 == 0 {
		if err := service.UpdateStreamingMessage(projectPath, backendName, sessionID, serializeBlocks()); err != nil {
			slog.Error("failed to update streaming message", slog.String("session", sessionID), slog.String("err", err.Error()))
		}
	}
	return nil
}

func executeStreamEventLoop(
	ctx context.Context,
	streamCh chan<- ai.StreamEvent,
	projectPath, backendName, sessionID, agentID string,
	eventCh <-chan ai.StreamEvent,
	wallStart time.Time,
) streamRunResult {
	state := &streamRunState{}

	flushTicker := time.NewTicker(1 * time.Second)
	defer flushTicker.Stop()

	serializeBlocks := func() string {
		contentMap := map[string]any{jsonKeyBlocks: state.blocks}
		if state.responseMetadata != nil {
			contentMap[jsonKeyMetadata] = state.responseMetadata
		}
		blocksJSON, _ := json.Marshal(contentMap)
		return string(blocksJSON)
	}

	for {
		select {
		case event, ok := <-eventCh:
			if !ok || event.Type == jsonKeyDone {
				return finalizeStreamRun(ctx, streamCh, projectPath, backendName, sessionID, agentID,
					ai.ChatRequest{}, state.blocks, state.responseMetadata, state.rawOutput, eventCh, wallStart)
			}
			if result := processStreamEvent(ctx, streamCh, projectPath, backendName, sessionID, agentID,
				state, eventCh, wallStart, serializeBlocks, event); result != nil {
				return *result
			}
		case <-ctx.Done():
			slog.Info("executeStreamRun context canceled, finalizing stream",
				slog.String("session", sessionID),
				slog.String("reason", ctx.Err().Error()))
			return finalizeStreamRun(ctx, streamCh, projectPath, backendName, sessionID, agentID,
				ai.ChatRequest{}, state.blocks, state.responseMetadata, state.rawOutput, eventCh, wallStart)
		case <-flushTicker.C:
			if len(state.blocks) > 0 {
				if err := service.UpdateStreamingMessage(projectPath, backendName, sessionID, serializeBlocks()); err != nil {
					slog.Error("failed to update streaming message", slog.String("session", sessionID), slog.String("err", err.Error()))
				}
			}
		}
	}
}

// handleStreamEvent processes non-forwarded events (raw_output, session_capture, resume_split).
// Returns true if the event was fully handled and should not be forwarded to SSE.
func handleStreamEvent(backendName, sessionID string, state *streamRunState, event ai.StreamEvent) bool {
	if event.Type == "raw_output" {
		state.rawOutput = event.RawOutput
		return true
	}
	if event.Type == "session_capture" {
		captureExternalSessionID(backendName, sessionID, event.Content)
		return true
	}
	return false
}

// captureExternalSessionID persists an external session ID for backends that use their own format.
func captureExternalSessionID(backendName, sessionID, extID string) {
	if extID == "" {
		return
	}
	if backendName != jsonKeyOpenCode && backendName != jsonKeyCodex && backendName != jsonKeyDeepSeek && backendName != "pi" {
		return
	}
	existingExtID := service.GetExternalSessionID(sessionID)
	if existingExtID != "" {
		return
	}
	if err := service.UpdateExternalSessionID(sessionID, extID); err != nil {
		slog.Error("failed to save external session ID", slog.String("session", sessionID), slog.String("external_id", extID), slog.String("err", err.Error()))
	} else {
		slog.Info("early-captured external session ID", slog.String("session", sessionID), slog.String("external_id", extID))
	}
}

// handleResumeSplit handles the resume_split event by finalizing the current message and starting a new one.
func handleResumeSplit(projectPath, backendName, sessionID string, state *streamRunState, serializeBlocks func() string) {
	slog.Info("resume_split received, finalizing current message and starting new one", slog.String("session", sessionID))

	if err := service.FinalizeStreamingMessage(projectPath, backendName, sessionID, serializeBlocks()); err != nil {
		slog.Error("failed to finalize pre-resume message", slog.String("session", sessionID), slog.String("err", err.Error()))
	}

	if state.rawOutput != "" {
		if msgID := service.GetStreamingMessageID(sessionID); msgID > 0 {
			if err := service.SaveRawResponse(sessionID, backendName, msgID, state.rawOutput); err != nil {
				slog.Error("failed to save raw response", slog.String("session", sessionID), slog.String("err", err.Error()))
			}
		}
		state.rawOutput = ""
	}

	state.blocks = nil
	state.responseMetadata = nil
	state.eventCount = 0

	emptyContent, _ := json.Marshal(map[string]any{jsonKeyBlocks: []any{}})
	if _, err := service.AddChatMessage(projectPath, backendName, sessionID, roleAssistant, string(emptyContent), nil, true, ""); err != nil {
		slog.Error("failed to create resume streaming message", slog.String("session", sessionID), slog.String("err", err.Error()))
	}
}

// finalizeStreamRun handles the finalize phase of a stream run: ask-question detection,
// DB finalization, raw output saving, and determining the result.
// It does NOT send a terminal SSE event.
func finalizeStreamRun(
	ctx context.Context,
	streamCh chan<- ai.StreamEvent,
	projectPath, backendName, sessionID, _ string,
	_ ai.ChatRequest,
	blocks []model.ContentBlock,
	responseMetadata *ai.Metadata,
	rawOutput string,
	eventCh <-chan ai.StreamEvent,
	wallStart time.Time,
) streamRunResult {
	// Detect <ask-question> in the fully accumulated text blocks and convert to tool_use blocks.
	if stringsContainsAnyBlock(blocks, "<ask-question") {
		slog.Info("detected ask-question tag(s) in accumulated text blocks", slog.String("session", sessionID))
		blocks = convertAskQuestionBlocks(blocks)
	}

	blocks = removeRejectedToolBlocks(blocks)

	wallMs := int(time.Since(wallStart).Milliseconds())
	if responseMetadata == nil {
		responseMetadata = &ai.Metadata{}
	}
	responseMetadata.WallMs = wallMs

	cancelReason := service.GetAndClearCancelReason(sessionID)

	content := buildFinalizedContent(ctx, blocks, responseMetadata, cancelReason)

	if err := service.FinalizeStreamingMessage(projectPath, backendName, sessionID, content); err != nil {
		slog.Error("failed to finalize streaming message", slog.String("session", sessionID), slog.String("err", err.Error()))
	}

	drainRawOutput(eventCh, &rawOutput)
	saveRawOutput(sessionID, backendName, rawOutput)

	result := buildStreamRunResult(ctx, cancelReason, blocks)

	slog.Info(
		"ai stream run done",
		slog.String("session", sessionID),
		slog.Int("blocks", len(blocks)),
		slog.String("cancel_reason", cancelReason),
		slog.Int("wall_ms", wallMs),
	)

	sendEvent(ctx, streamCh, ai.StreamEvent{Type: jsonKeyMetadata, Meta: responseMetadata})

	return result
}

// buildFinalizedContent serializes the blocks and metadata into the final JSON content for DB storage.
func buildFinalizedContent(ctx context.Context, blocks []model.ContentBlock, responseMetadata *ai.Metadata, cancelReason string) string {
	if len(blocks) == 0 {
		return buildEmptyContent(ctx, cancelReason)
	}
	contentMap := map[string]any{jsonKeyBlocks: blocks}
	if responseMetadata != nil {
		contentMap[jsonKeyMetadata] = responseMetadata
	}
	switch {
	case cancelReason == jsonKeyUser:
		contentMap["cancelled"] = true
	case ctx.Err() == context.Canceled:
		contentMap["cancelled"] = true
	case ctx.Err() == context.DeadlineExceeded:
		blocks = append(blocks, model.ContentBlock{Type: jsonKeyWarning, Text: timeoutMsg30Min, Reason: ai.ReasonTimeout})
	}
	contentMap["blocks"] = blocks
	blocksJSON, _ := json.Marshal(contentMap)
	return string(blocksJSON)
}

// buildEmptyContent generates content for an empty (no blocks) response.
func buildEmptyContent(ctx context.Context, cancelReason string) string {
	var errMsg, reason string
	switch {
	case cancelReason == jsonKeyUser:
		errMsg, reason = "User canceled", ai.ReasonUserCancel
	case ctx.Err() == context.Canceled:
		errMsg, reason = "AI response canceled", ai.ReasonContextCancel
	case ctx.Err() == context.DeadlineExceeded:
		errMsg, reason = timeoutMsg30Min, ai.ReasonTimeout
	default:
		errMsg, reason = "AI returned no content", ai.ReasonEmpty
	}
	blocks := []model.ContentBlock{{Type: jsonKeyWarning, Text: errMsg, Reason: reason}}
	contentMap := map[string]any{jsonKeyBlocks: blocks}
	if cancelReason == jsonKeyUser || ctx.Err() == context.Canceled {
		contentMap["cancelled"] = true
	}
	blocksJSON, _ := json.Marshal(contentMap)
	return string(blocksJSON)
}

// drainRawOutput drains remaining events from the channel to capture any raw_output.
func drainRawOutput(eventCh <-chan ai.StreamEvent, rawOutput *string) {
	for {
		select {
		case event, ok := <-eventCh:
			if !ok {
				return
			}
			if event.Type == "raw_output" && *rawOutput == "" {
				*rawOutput = event.RawOutput
			}
		default:
			return
		}
	}
}

// saveRawOutput persists raw AI backend output for debugging if available.
func saveRawOutput(sessionID, backendName, rawOutput string) {
	if rawOutput == "" {
		return
	}
	if msgID := service.GetStreamingMessageID(sessionID); msgID > 0 {
		if err := service.SaveRawResponse(sessionID, backendName, msgID, rawOutput); err != nil {
			slog.Error("failed to save raw response", slog.String("session", sessionID), slog.String("err", err.Error()))
		}
	}
}

// buildStreamRunResult determines the result status based on cancel reason and context.
func buildStreamRunResult(ctx context.Context, cancelReason string, blocks []model.ContentBlock) streamRunResult {
	result := streamRunResult{}
	switch {
	case cancelReason == jsonKeyUser:
		result.cancelReason = cancelReason
	case ctx.Err() == context.Canceled:
		result.cancelReason = "cancel"
	case ctx.Err() == context.DeadlineExceeded:
		result.err = timeoutMsg30Min
	case len(blocks) == 0:
		result.empty = true
	}
	return result
}

// buildChatRequest constructs an ai.ChatRequest from the given parameters.
// modelOverride, if non-empty, takes precedence over the agent's default model.
// thinkingEffortOverride, if non-empty, takes precedence over the agent's YAML default.
func buildChatRequest(prompt, sessionID, projectPath, backendName, agentID, modelOverride, thinkingEffortOverride, fileDir string) ai.ChatRequest {
	systemPrompt := ""
	agentModel := ""
	agentCommand := ""
	effectiveThinkingEffort := thinkingEffortOverride // Frontend selection takes priority

	if agentID == "" {
		agentID = model.GetDefaultAgentID()
	}
	if agent, ok := model.Agents[agentID]; ok {
		systemPrompt = agent.SystemPrompt
		// Replace {{PROJECT_PATH}} per-request with the actual project path from cookie
		if projectPath != "" {
			systemPrompt = strings.ReplaceAll(systemPrompt, "{{PROJECT_PATH}}", projectPath)
		}
		if modelOverride != "" {
			agentModel = modelOverride
		} else if defaultID := agent.DefaultModelID(); defaultID != "" {
			agentModel = defaultID
		}
		if agent.Command != "" {
			agentCommand = agent.Command
		}
		// Fall back to agent's effective thinking effort when frontend didn't specify
		if effectiveThinkingEffort == "" && agent.EffectiveThinkingEffort() != "" {
			effectiveThinkingEffort = agent.EffectiveThinkingEffort()
		}
	}

	// For backends that use their own session ID format (not ClawBench UUID),
	// resolve external session ID when resuming.
	effectiveSessionID := sessionID
	resume := service.SessionHasAssistant(sessionID)
	if (backendName == jsonKeyOpenCode || backendName == jsonKeyCodex || backendName == jsonKeyDeepSeek || backendName == "pi") && resume {
		extID := service.GetExternalSessionID(sessionID)
		if extID != "" {
			effectiveSessionID = extID
		} else {
			// No external session ID available — don't pass the invalid ClawBench UUID
			// to these CLIs. They don't recognize it and would fail
			// (stdout empty or error), resulting in "AI returned no content" or
			// "could not load session" errors.
			// Let them start a fresh session instead.
			effectiveSessionID = ""
		}
	}

	return ai.ChatRequest{
		Prompt:                prompt,
		SessionID:             effectiveSessionID,
		WorkDir:               fileDir,
		SystemPrompt:          systemPrompt,
		Model:                 agentModel,
		Command:               agentCommand,
		AgentID:               agentID,
		ThinkingEffort:        effectiveThinkingEffort,
		Resume:                resume,
		AssistantMessageCount: service.GetAssistantMessageCount(sessionID),
	}
}

// buildChatRequestFromQueue constructs an ai.ChatRequest from a queued message.
func buildChatRequestFromQueue(qMsg model.QueuedMessage, sessionID, projectPath, backendName, agentID, fileDir string) ai.ChatRequest {
	prompt := qMsg.Text
	if len(qMsg.FilePaths) > 0 {
		basePath, _ := filepath.Abs(projectPath)
		var filePaths, dirPaths []string
		for _, fp := range qMsg.FilePaths {
			absPath, ok := model.ValidatePath(basePath, fp)
			if !ok {
				filePaths = append(filePaths, fp)
				continue
			}
			info, err := os.Stat(absPath)
			if err != nil {
				filePaths = append(filePaths, fp)
				continue
			}
			if info.IsDir() {
				dirPaths = append(dirPaths, absPath)
			} else {
				filePaths = append(filePaths, absPath)
			}
		}
		if len(filePaths) > 0 {
			prompt = fmt.Sprintf("[Current file: %s]\n%s", strings.Join(filePaths, ", "), qMsg.Text)
		}
		if len(dirPaths) > 0 {
			prompt = fmt.Sprintf("[Current directory: %s]\n%s", strings.Join(dirPaths, ", "), prompt)
		}
	}
	if len(qMsg.Files) > 0 {
		prompt = fmt.Sprintf("[User uploaded %d file(s): %s]\n%s", len(qMsg.Files), strings.Join(qMsg.Files, ", "), prompt)
	}

	// Use session-persisted model (if user explicitly chose one) as modelOverride
	// so queued messages respect the user's model choice, not just the agent default.
	sessionModel := service.GetSessionModel(sessionID)
	return buildChatRequest(prompt, sessionID, projectPath, backendName, agentID, sessionModel, service.GetSessionThinkingEffort(sessionID), fileDir)
}

// CancelChat handles POST to cancel an ongoing AI stream for a session.
func CancelChat(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
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

	// Verify the session belongs to the requesting project
	if sessionProject := service.GetSessionProjectPath(sessionID); sessionProject != projectPath {
		writeLocalizedError(w, r, model.Forbidden(nil, "AccessDenied"))
		return
	}

	if !service.CancelSession(sessionID) {
		writeLocalizedErrorf(w, r, http.StatusNotFound, "SessionNotRunning")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// stringsContainsAnyBlock checks if any text ContentBlock contains the given substring.
func stringsContainsAnyBlock(blocks []model.ContentBlock, substr string) bool {
	for _, b := range blocks {
		if b.Type == "text" && strings.Contains(b.Text, substr) {
			return true
		}
	}
	return false
}

// extractJSONCandidate prepares a raw <ask-question> content string for JSON
// parsing. It strips markdown code fences and trailing XML closing tags that
// some models append after the JSON payload (e.g. "</user_query>"). Returns
// the cleaned JSON string if the content looks like valid JSON (starts with
// '{' or '['), or an empty string otherwise.
func extractJSONCandidate(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// Strip markdown code fences (```json ... ```)
	if strings.HasPrefix(trimmed, "```") {
		if nl := strings.Index(trimmed, "\n"); nl != -1 {
			trimmed = strings.TrimSpace(trimmed[nl+1:])
		}
		if idx := strings.LastIndex(trimmed, "```"); idx != -1 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
	}
	// Strip trailing XML closing tags that some models incorrectly append
	// after the JSON payload (e.g. GLM-5.1 uses </user_query>).
	reTrailingXML := regexp.MustCompile(`\s*</[a-zA-Z_][\w.-]*>\s*$`)
	for reTrailingXML.MatchString(trimmed) {
		trimmed = strings.TrimSpace(reTrailingXML.ReplaceAllString(trimmed, ""))
	}
	// Fallback: strip trailing closing tags with non-ASCII/obfuscated characters
	// (e.g. </｜｜DSML｜｜question> with fullwidth pipe U+FF5C). The strict regex
	// above won't match these, so use a permissive pattern as a second pass.
	reTrailingXMLLoose := regexp.MustCompile(`\s*</[^>]+>\s*$`)
	for reTrailingXMLLoose.MatchString(trimmed) {
		prev := trimmed
		trimmed = strings.TrimSpace(reTrailingXMLLoose.ReplaceAllString(trimmed, ""))
		if trimmed == prev {
			break
		}
	}
	// Strip leading XML tags that some models use to wrap the JSON payload
	// (e.g. <parameter name="questions">). These are parameter-style wrappers
	// that enclose the JSON array/dict instead of placing it directly.
	reLeadingXML := regexp.MustCompile(`^\s*<[a-zA-Z_][\w.-]*(?:\s[^>]*)?>\s*`)
	if reLeadingXML.MatchString(trimmed) {
		trimmed = strings.TrimSpace(reLeadingXML.ReplaceAllString(trimmed, ""))
	}
	// Validate that the content looks like JSON — must start with '{' or '['.
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	return trimmed
}

// convertAskQuestionBlocks detects <ask-question> tags in text ContentBlocks,
// parses the JSON content, and converts them into tool_use ContentBlocks with
// name="AskUserQuestion". Tags are stripped from text; if no text remains the
// block is replaced entirely, otherwise a new tool_use block is appended.
//
// Tolerates three closing-tag variants:
//  1. Standard </ask-question>
//  2. Non-standard closing tags (e.g. </user_query>, obfuscated tags)
//  3. No closing tag at all (tag runs to end-of-text)
//
// Returns the updated blocks slice.
func convertAskQuestionBlocks(blocks []model.ContentBlock) []model.ContentBlock {
	// Pre-compiled regexes for the three matching strategies.
	reStandard := regexp.MustCompile(`<ask-question\b[^>]*>([\s\S]*?)</ask-question>`)
	reWrongClose := regexp.MustCompile(`<ask-question\b[^>]*>([\s\S]*?)</[^>]+>`)
	reUnclosed := regexp.MustCompile(`<ask-question\b[^>]*>([\s\S]+)$`)

	// First pass: collect all conversions needed
	type conversion struct {
		index     int
		input     map[string]any
		cleanText string
	}
	var conversions []conversion

	for i, block := range blocks {
		if block.Type != "text" || !strings.Contains(block.Text, "<ask-question") {
			continue
		}

		jsonContent, tagStart, tagEnd := findAskMatch(block.Text, reStandard, reWrongClose, reUnclosed)
		if jsonContent == "" {
			continue
		}

		input, ok := parseAskQuestionJSON(jsonContent)
		if !ok {
			continue
		}

		cleanText := strings.TrimSpace(block.Text[:tagStart] + block.Text[tagEnd:])
		conversions = append(conversions, conversion{index: i, input: input, cleanText: cleanText})
	}

	// Apply conversions in reverse order so index shifts don't affect earlier entries
	for i := len(conversions) - 1; i >= 0; i-- {
		c := conversions[i]
		toolBlock := model.ContentBlock{
			Type:  jsonKeyToolUse,
			Name:  toolNameAskUserQuestion,
			ID:    "ask-" + uuid.New().String(),
			Input: c.input,
			Done:  true,
		}

		if c.cleanText == "" {
			blocks[c.index] = toolBlock
		} else {
			blocks[c.index].Text = c.cleanText
			insertAt := c.index + 1
			blocks = append(blocks[:insertAt], append([]model.ContentBlock{toolBlock}, blocks[insertAt:]...)...)
		}
	}

	blocks = removeRejectedToolBlocks(blocks)

	return blocks
}

// findAskMatch tries three regex strategies (from strict to loose) to locate
// a valid <ask-question> tag in text. Returns the JSON content string and
// the [start, end) byte positions of the full tag span in text (for removal).
func findAskMatch(text string, reStandard, reWrongClose, reUnclosed *regexp.Regexp) (content string, start, end int) {
	for _, re := range []*regexp.Regexp{reStandard, reWrongClose, reUnclosed} {
		matches := re.FindAllStringSubmatchIndex(text, -1)
		for j := len(matches) - 1; j >= 0; j-- {
			pair := matches[j]
			if candidate := extractJSONCandidate(text[pair[2]:pair[3]]); candidate != "" {
				return candidate, pair[0], pair[1]
			}
		}
	}
	return "", -1, -1
}

// parseAskQuestionJSON parses the JSON content from an <ask-question> tag into a map.
func parseAskQuestionJSON(jsonContent string) (map[string]any, bool) {
	var input map[string]any
	if err := json.Unmarshal([]byte(jsonContent), &input); err != nil {
		var questionsArr []any
		if err2 := json.Unmarshal([]byte(jsonContent), &questionsArr); err2 == nil && len(questionsArr) > 0 {
			input = map[string]any{"questions": questionsArr}
		} else {
			slog.Error("failed to parse ask-question JSON", slog.String("error", err.Error()))
			return nil, false
		}
	}

	questions, ok := input["questions"]
	if !ok {
		slog.Error("ask-question missing 'questions' field")
		return nil, false
	}
	questionsArr, ok := questions.([]any)
	if !ok || len(questionsArr) == 0 {
		slog.Error("ask-question 'questions' must be a non-empty array")
		return nil, false
	}
	return input, true
}

// removeRejectedToolBlocks strips tool_use blocks that were rejected by the CLI
// (Status=="error" and output contains "not found in agent cli"). These occur when
// the AI model hallucinates tool names (e.g. "/commit" as a slash command, or
// "AskUserQuestion" when <ask-question> XML tags are also emitted). The rejected
// tool_use block and its matching warning are confusing noise for the user.
// Also removes warning blocks containing the "Tool <name> not found in agent cli" pattern.
func removeRejectedToolBlocks(blocks []model.ContentBlock) []model.ContentBlock {
	// Collect names of rejected tools from failed tool_use blocks
	rejectedNames := make(map[string]bool)
	for _, block := range blocks {
		if block.Type == jsonKeyToolUse && block.Status == "error" && strings.Contains(block.Output, "not found in agent cli") {
			rejectedNames[block.Name] = true
		}
	}
	if len(rejectedNames) == 0 {
		return blocks
	}

	filtered := make([]model.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		// Remove failed tool_use blocks for rejected tool names
		if block.Type == jsonKeyToolUse && block.Status == "error" && rejectedNames[block.Name] {
			slog.Info(
				"removing rejected tool_use block from CLI",
				slog.String("name", block.Name),
				slog.String("id", block.ID),
				slog.String("output", block.Output),
			)
			continue
		}
		// Remove warning blocks that reference the rejected tool name with "not found"
		if block.Type == jsonKeyWarning && strings.Contains(block.Text, "not found") {
			matched := false
			for name := range rejectedNames {
				if strings.Contains(block.Text, name) {
					matched = true
					break
				}
			}
			if matched {
				slog.Info(
					"removing rejected-tool warning block",
					slog.String("text", block.Text),
				)
				continue
			}
		}
		filtered = append(filtered, block)
	}
	return filtered
}

// sendEvent sends an event to the stream channel.
// Non-blocking: if the channel is full (no SSE client reading), the event is dropped.
// This is safe because content is persisted to DB independently.
func sendEvent(ctx context.Context, ch chan<- ai.StreamEvent, event ai.StreamEvent) bool {
	select {
	case ch <- event:
		return true
	case <-ctx.Done():
		return false
	default:
		// Channel full — drop the event, DB persistence ensures no data loss
		toolID := ""
		if event.Tool != nil {
			toolID = event.Tool.ID
		}
		slog.Warn(
			"SSE event dropped — channel full",
			slog.String("type", event.Type),
			slog.String("tool_id", toolID),
		)
		return true
	}
}

// sendFinalEvent sends a terminal event (done/canceled/error) to the stream channel
// without checking context cancellation. This ensures the SSE client always receives
// the terminal event even after the CLI context has been canceled (e.g. ExitPlanMode).
func sendFinalEvent(ch chan<- ai.StreamEvent, event ai.StreamEvent) {
	select {
	case ch <- event:
	default:
		slog.Warn(
			"SSE terminal event dropped — channel full",
			slog.String("type", event.Type),
		)
	}
}
