package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"clawbench/internal/model"
	"clawbench/internal/service"
)

// ServeSessions handles GET (list) and POST (create) for chat sessions.
func ServeSessions(w http.ResponseWriter, r *http.Request) {
	projectPath, ok := requireProject(w, r)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet:
		serveSessionsGet(w, r, projectPath)
	case http.MethodPost:
		serveSessionsPost(w, r, projectPath)
	default:
		writeLocalizedErrorf(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func serveSessionsGet(w http.ResponseWriter, r *http.Request, projectPath string) {
	limit, cursor, cursorID := parseSessionPagination(r)

	var sessions []model.ChatSession
	var hasMore bool
	var err error

	if limit > 0 {
		sessions, hasMore, err = service.GetSessionsPaged(projectPath, "", limit, cursor, cursorID)
	} else {
		sessions, err = service.GetSessions(projectPath, "")
	}
	if err != nil {
		model.WriteError(w, model.Internal(fmt.Errorf("failed to load sessions")))
		return
	}

	runningIDs := service.GetRunningSessionIDs()
	runningSet := make(map[string]bool, len(runningIDs))
	for _, id := range runningIDs {
		runningSet[id] = true
	}
	for i := range sessions {
		sessions[i].Running = runningSet[sessions[i].ID]
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessions":     sessions,
		jsonKeyHasMore: hasMore,
	})
}

func parseSessionPagination(r *http.Request) (limit int, cursor, cursorID string) {
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	cursor = r.URL.Query().Get("cursor")
	cursorID = r.URL.Query().Get("cursor_id")
	if cursor != "" {
		cursor = strings.ReplaceAll(cursor, "T", " ")
		cursor = strings.TrimSuffix(cursor, "Z")
		cursor = strings.TrimSuffix(cursor, "+00:00")
	}
	return
}

func serveSessionsPost(w http.ResponseWriter, r *http.Request, projectPath string) {
	if model.SessionMaxCount > 0 {
		if count, cerr := service.GetSessionCount(projectPath); cerr == nil && count >= model.SessionMaxCount {
			writeLocalizedErrorf(w, r, http.StatusConflict, "SessionLimitReached", map[string]any{"MaxCount": model.SessionMaxCount})
			return
		}
	}

	var req struct {
		Title   string `json:"title"`
		Backend string `json:"backend"`
		AgentID string `json:"agentId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodySize)
	if !decodeJSON(w, r, &req) {
		return
	}

	resolvedAgentID, backend, agentSource := resolveSessionAgent(req.AgentID, req.Backend)
	title := resolveSessionTitle(r, projectPath, req.Title, backend)

	sessionID, err := service.CreateSession(projectPath, backend, title, resolvedAgentID, "", agentSource, "chat")
	if err != nil {
		model.WriteError(w, model.Internal(fmt.Errorf("failed to create session")))
		return
	}
	setSessionID(w, sessionID)
	sessionCount, _ := service.GetSessionCount(projectPath)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, jsonKeySessionID: sessionID, jsonKeyBackend: backend, jsonKeyAgentID: resolvedAgentID, "sessionCount": sessionCount, "title": title})
}

func resolveSessionAgent(agentID, reqBackend string) (resolvedAgentID, backend, agentSource string) {
	resolvedAgentID = agentID
	agentSource = "default"
	backend2, _, _, _, ok := resolveAgentConfig(agentID)
	if !ok {
		return "", "", ""
	}
	if backend2 != "" {
		reqBackend = backend2
	}
	if resolvedAgentID == "" {
		resolvedAgentID = model.GetDefaultAgentID()
	}
	if agentID != "" {
		agentSource = jsonKeyUser
	}
	if reqBackend == "" {
		reqBackend = jsonKeyCodebuddy
	}
	return resolvedAgentID, reqBackend, agentSource
}

func resolveSessionTitle(r *http.Request, projectPath, reqTitle, backend string) string {
	if reqTitle != "" {
		return reqTitle
	}
	existingSessions, err := service.GetSessions(projectPath, backend)
	if err == nil {
		return T(r, "NewSessionN", map[string]any{"N": len(existingSessions) + 1})
	}
	return T(r, "NewSession")
}

// DeleteSession handles DELETE for a single session.
func DeleteSession(w http.ResponseWriter, r *http.Request) {
	projectPath, ok := requireProject(w, r)
	if !ok {
		return
	}

	if !requireMethod(w, r, http.MethodDelete) {
		return
	}

	sessionID, ok := requireSessionID(w, r)
	if !ok {
		return
	}

	backend := r.URL.Query().Get("backend")
	if backend == "" {
		backend = "codebuddy"
	}

	if err := service.DeleteSession(projectPath, backend, sessionID); err != nil {
		model.WriteError(w, model.Internal(fmt.Errorf("failed to delete session")))
		return
	}

	sessionCount, _ := service.GetSessionCount(projectPath)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "sessionCount": sessionCount})
}

// getSessionID retrieves session ID from query param or cookie.
func getSessionID(r *http.Request) string {
	if sessionID := r.URL.Query().Get("session_id"); sessionID != "" {
		return sessionID
	}
	cookie, err := r.Cookie("chat_session_id")
	if err != nil {
		return ""
	}
	return cookie.Value
}

// setSessionID sets session ID in cookie.
// HttpOnly: true prevents JavaScript access, mitigating XSS-based session hijack (ISS-123).
func setSessionID(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // local network only, no HTTPS; Secure flag would prevent functionality
		Value:    sessionID,
		Path:     "/",
		MaxAge:   86400 * 30, // 30 days
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
