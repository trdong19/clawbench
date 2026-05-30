package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"clawbench/internal/model"
	"clawbench/internal/platform"
	"clawbench/internal/service"
)

// ServeRecentProjects handles GET (list) and POST (add) for recent projects.
func ServeRecentProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		paths, err := service.GetRecentProjects()
		if err != nil {
			model.WriteError(w, model.Internal(fmt.Errorf("failed to load recent projects")))
			return
		}
		writeJSON(w, http.StatusOK, paths)

	case http.MethodPost:
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidRequestBody")
			return
		}
		if err := service.AddRecentProject(req.Path); err != nil {
			model.WriteError(w, model.Internal(fmt.Errorf("failed to save recent project")))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidRequestBody")
			return
		}
		if err := service.RemoveRecentProject(req.Path); err != nil {
			model.WriteError(w, model.Internal(fmt.Errorf("failed to remove recent project")))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		writeLocalizedErrorf(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// ServeProjectSet handles GET (current project) and POST (set project).
func ServeProjectSet(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cookie, err := r.Cookie("clawbench_project")
		projectPath := ""
		if err == nil && cookie.Value != "" {
			decoded, decErr := url.QueryUnescape(cookie.Value)
			if decErr == nil {
				projectPath = decoded
			} else {
				projectPath = cookie.Value
			}
		} else {
			recents, _ := service.GetRecentProjects()
			if len(recents) > 0 {
				projectPath = recents[0]
			} else if len(model.RootPaths) > 0 {
				projectPath = model.RootPaths[0]
			}
		}
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // local network only, no HTTPS; Secure flag would prevent functionality
			Name:     "clawbench_project",
			Value:    url.QueryEscape(projectPath),
			Path:     "/",
			MaxAge:   7 * 24 * 3600,
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, http.StatusOK, map[string]string{jsonKeyPath: projectPath, "homeDir": platform.UserHomeDir()})

	case http.MethodPost:
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidRequestBody")
			return
		}

		// Resolve path and validate against root paths
		rawPath := req.Path
		absPath, ok := resolveProjectSetPath(rawPath)
		if !ok {
			writeLocalizedError(w, r, model.Forbidden(nil, "AccessDenied"))
			return
		}
		if absPath == "" {
			writeLocalizedErrorf(w, r, http.StatusBadRequest, "InvalidPath")
			return
		}

		info, err := os.Stat(absPath)
		if err != nil || !info.IsDir() {
			writeLocalizedErrorf(w, r, http.StatusBadRequest, "NotADirectory")
			return
		}

		// Clear chat session cookie when switching project
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // local network only, no HTTPS; Secure flag would prevent functionality
			Name:     "chat_session_id",
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: false,
			SameSite: http.SameSiteLaxMode,
		})

		http.SetCookie(w, &http.Cookie{ //nolint:gosec // local network only, no HTTPS; Secure flag would prevent functionality
			Name:     "clawbench_project",
			Value:    url.QueryEscape(absPath),
			Path:     "/",
			MaxAge:   7 * 24 * 3600,
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true", jsonKeyPath: absPath})

	default:
		writeLocalizedErrorf(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// resolveProjectSetPath resolves a raw path for project setting.
// Returns (absPath, true) if valid, ("", false) if access denied.
func resolveProjectSetPath(rawPath string) (string, bool) {
	switch {
	case rawPath == "" || rawPath == "/":
		if len(model.RootPaths) > 0 {
			return model.RootPaths[0], true
		}
		return "", false
	case filepath.IsAbs(rawPath):
		if !isPathUnderAnyRoot(rawPath) {
			return "", false
		}
		return rawPath, true
	default:
		if len(model.RootPaths) == 0 {
			return "", false
		}
		relPath := strings.TrimPrefix(rawPath, "/")
		absPath, _ := filepath.Abs(filepath.Join(model.RootPaths[0], relPath))
		if !isPathUnderAnyRoot(absPath) {
			return "", false
		}
		return absPath, true
	}
}

// ServeRoots returns the filesystem root paths and configuration limits as JSON.
// On Linux/macOS, roots is ["/"]. On Windows, roots is the list of available drives.
func ServeRoots(w http.ResponseWriter, _ *http.Request) {
	roots := model.RootPaths
	if len(roots) == 0 {
		slog.Warn("no root paths configured")
		roots = []string{platform.UserHomeDir()}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"roots":                  roots,
		"uploadMaxSizeMB":        model.UploadMaxSizeMB,
		"uploadMaxFiles":         model.UploadMaxFiles,
		"chatInitialMessages":    model.ChatInitialMessages,
		"chatPageSize":           model.ChatPageSize,
		"chatSessionPageSize":    model.ChatSessionPageSize,
		"chatCollapsedHeight":    model.ChatCollapsedHeight,
		"sessionMaxCount":        model.SessionMaxCount,
		"recentProjectsMaxCount": model.RecentProjectsMaxCount,
	})
}
