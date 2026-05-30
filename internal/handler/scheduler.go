package handler

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"clawbench/internal/model"
	"clawbench/internal/service"
)

// ServeTasks handles GET (list) and POST (create) for scheduled tasks.
func ServeTasks(w http.ResponseWriter, r *http.Request) {
	projectPath, ok := requireProject(w, r)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet:
		serveTasksGet(w, r, projectPath)
	case http.MethodPost:
		serveTasksPost(w, r, projectPath)
	default:
		writeLocalizedErrorf(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func serveTasksGet(w http.ResponseWriter, _ *http.Request, projectPath string) {
	tasks, err := service.GetTasks(projectPath)
	if err != nil {
		model.WriteError(w, model.Internal(fmt.Errorf("failed to load tasks")))
		return
	}
	if tasks == nil {
		tasks = []model.ScheduledTask{}
	}
	runningCounts := service.GlobalScheduler.GetRunningCounts()
	for i := range tasks {
		tasks[i].RunningCount = runningCounts[tasks[i].ID]
	}
	hasUnread := false
	for _, t := range tasks {
		if t.UnreadCount > 0 {
			hasUnread = true
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "hasUnread": hasUnread})
}

func serveTasksPost(w http.ResponseWriter, r *http.Request, projectPath string) {
	var req struct {
		Name       string `json:"name"`
		CronExpr   string `json:"cron_expr"`
		AgentID    string `json:"agent_id"`
		Prompt     string `json:"prompt"`
		RepeatMode string `json:"repeat_mode"`
		MaxRuns    int    `json:"max_runs"`
		SessionID  string `json:"session_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.CronExpr == "" || req.AgentID == "" || req.Prompt == "" {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "TaskFieldsRequired")
		return
	}
	if req.RepeatMode == "" {
		req.RepeatMode = "unlimited"
	}

	task := &model.ScheduledTask{
		ProjectPath: projectPath,
		Name:        req.Name,
		CronExpr:    req.CronExpr,
		AgentID:     req.AgentID,
		Prompt:      req.Prompt,
		RepeatMode:  req.RepeatMode,
		MaxRuns:     req.MaxRuns,
		SessionID:   req.SessionID,
	}

	if err := service.GlobalScheduler.AddTask(task); err != nil {
		model.WriteError(w, model.Internal(fmt.Errorf("failed to create task: %w", err)))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": task})
}

// ServeTaskByID handles operations on a single task by ID.
// GET /api/tasks/{id} - get task details
// PUT /api/tasks/{id} - update task (pause/resume)
// DELETE /api/tasks/{id} - delete task
// GET /api/tasks/{id}/executions - get execution history
func ServeTaskByID(w http.ResponseWriter, r *http.Request) {
	projectPath, ok := requireProject(w, r)
	if !ok {
		return
	}

	taskID, subPath, ok := parseTaskPath(w, r)
	if !ok {
		return
	}

	if subPath == "executions" && r.Method == http.MethodGet {
		serveTaskExecutions(w, r, taskID, projectPath)
		return
	}

	switch r.Method {
	case http.MethodGet:
		serveTaskByIDGet(w, r, taskID, projectPath)
	case http.MethodPut:
		serveTaskByIDPut(w, r, taskID, projectPath)
	case http.MethodDelete:
		serveTaskByIDDelete(w, r, taskID, projectPath)
	default:
		writeLocalizedErrorf(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// parseTaskPath extracts task ID and sub-path from the URL.
func parseTaskPath(w http.ResponseWriter, r *http.Request) (taskID int64, subPath string, ok bool) {
	path := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.SplitN(path, "/", 2)
	taskIDStr := parts[0]
	if len(parts) > 1 {
		subPath = parts[1]
	}

	if taskIDStr == "" {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "TaskIdRequired")
		return 0, "", false
	}

	var err error
	taskID, err = strconv.ParseInt(taskIDStr, 10, 64)
	if err != nil {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "TaskIdInvalid")
		return 0, "", false
	}
	return taskID, subPath, true
}

func serveTaskByIDGet(w http.ResponseWriter, r *http.Request, taskID int64, projectPath string) {
	task, err := service.GetTaskByID(taskID)
	if err != nil {
		writeLocalizedError(w, r, model.NotFound(nil, "TaskNotFound"))
		return
	}
	if task.ProjectPath != projectPath {
		writeLocalizedError(w, r, model.Forbidden(nil, "AccessDenied"))
		return
	}
	task.RunningExecutions = service.GlobalScheduler.GetRunningExecutions(taskID)
	task.RunningCount = len(task.RunningExecutions)
	writeJSON(w, http.StatusOK, task)
}

func serveTaskByIDPut(w http.ResponseWriter, r *http.Request, taskID int64, projectPath string) {
	var req struct {
		Action      string `json:"action"`
		ExecutionID string `json:"executionId"`
		Name        string `json:"name"`
		CronExpr    string `json:"cron_expr"`
		AgentID     string `json:"agent_id"`
		Prompt      string `json:"prompt"`
		RepeatMode  string `json:"repeat_mode"`
		MaxRuns     *int   `json:"max_runs"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	task, err := service.GetTaskByID(taskID)
	if err != nil {
		writeLocalizedError(w, r, model.NotFound(nil, "TaskNotFound"))
		return
	}
	if task.ProjectPath != projectPath {
		writeLocalizedError(w, r, model.Forbidden(nil, "AccessDenied"))
		return
	}

	// Handle action-based operations
	if handled := handleTaskAction(w, r, req, taskID); handled {
		return
	}

	// Full task update
	applyTaskUpdate(task, req)
	if err := service.GlobalScheduler.UpdateTask(task); err != nil {
		model.WriteError(w, model.Internal(fmt.Errorf("failed to update task: %w", err)))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": task})
}

// handleTaskAction processes action-based task operations. Returns true if the action was handled.
func handleTaskAction(w http.ResponseWriter, r *http.Request, req struct {
	Action      string `json:"action"`
	ExecutionID string `json:"executionId"`
	Name        string `json:"name"`
	CronExpr    string `json:"cron_expr"`
	AgentID     string `json:"agent_id"`
	Prompt      string `json:"prompt"`
	RepeatMode  string `json:"repeat_mode"`
	MaxRuns     *int   `json:"max_runs"`
}, taskID int64,
) bool {
	switch req.Action {
	case "pause":
		service.GlobalScheduler.PauseTask(taskID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return true
	case "resume":
		if err := service.GlobalScheduler.ResumeTask(taskID); err != nil {
			model.WriteError(w, model.Internal(err))
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return true
	case "read":
		return handleTaskRead(w, req, taskID)
	case "trigger":
		return handleTaskTrigger(w, r, taskID)
	case "cancel":
		return handleTaskCancel(w, r, req)
	case "deleteExecution":
		return handleDeleteExecution(w, r, req)
	case "deleteAllExecutions":
		return handleDeleteAllExecutions(w, taskID)
	}
	return false
}

func handleTaskRead(w http.ResponseWriter, req struct {
	Action      string `json:"action"`
	ExecutionID string `json:"executionId"`
	Name        string `json:"name"`
	CronExpr    string `json:"cron_expr"`
	AgentID     string `json:"agent_id"`
	Prompt      string `json:"prompt"`
	RepeatMode  string `json:"repeat_mode"`
	MaxRuns     *int   `json:"max_runs"`
}, taskID int64,
) bool {
	if req.ExecutionID != "" {
		if err := service.MarkExecutionRead(req.ExecutionID); err != nil {
			model.WriteError(w, model.Internal(err))
			return true
		}
	} else {
		if err := service.UpdateTaskLastRead(taskID); err != nil {
			model.WriteError(w, model.Internal(err))
			return true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return true
}

func handleTaskTrigger(w http.ResponseWriter, r *http.Request, taskID int64) bool {
	if service.GlobalScheduler.HasRunningExecutions(taskID) {
		writeLocalizedErrorf(w, r, http.StatusConflict, "TaskAlreadyRunning")
		return true
	}
	if err := service.GlobalScheduler.TriggerTask(taskID); err != nil {
		writeLocalizedError(w, r, model.NotFound(err, "TaskNotFound"))
		return true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return true
}

func handleTaskCancel(w http.ResponseWriter, r *http.Request, req struct {
	Action      string `json:"action"`
	ExecutionID string `json:"executionId"`
	Name        string `json:"name"`
	CronExpr    string `json:"cron_expr"`
	AgentID     string `json:"agent_id"`
	Prompt      string `json:"prompt"`
	RepeatMode  string `json:"repeat_mode"`
	MaxRuns     *int   `json:"max_runs"`
},
) bool {
	if req.ExecutionID == "" {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "TaskExecutionIdRequired")
		return true
	}
	if err := service.GlobalScheduler.CancelExecution(req.ExecutionID); err != nil {
		writeLocalizedError(w, r, model.NotFound(err, "TaskExecutionNotFound"))
		return true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return true
}

func handleDeleteExecution(w http.ResponseWriter, r *http.Request, req struct {
	Action      string `json:"action"`
	ExecutionID string `json:"executionId"`
	Name        string `json:"name"`
	CronExpr    string `json:"cron_expr"`
	AgentID     string `json:"agent_id"`
	Prompt      string `json:"prompt"`
	RepeatMode  string `json:"repeat_mode"`
	MaxRuns     *int   `json:"max_runs"`
},
) bool {
	if req.ExecutionID == "" {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "TaskExecutionIdRequired")
		return true
	}
	executionID, err := strconv.ParseInt(req.ExecutionID, 10, 64)
	if err != nil {
		writeLocalizedErrorf(w, r, http.StatusBadRequest, "TaskExecutionIdInvalid")
		return true
	}
	if err := service.DeleteTaskExecution(executionID); err != nil {
		switch {
		case strings.Contains(err.Error(), "not found"):
			writeLocalizedError(w, r, model.NotFound(err, "TaskExecutionNotFound"))
		case strings.Contains(err.Error(), "cannot delete a running"):
			writeLocalizedErrorf(w, r, http.StatusConflict, "TaskExecutionRunning")
		default:
			model.WriteError(w, model.Internal(err))
		}
		return true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return true
}

func handleDeleteAllExecutions(w http.ResponseWriter, taskID int64) bool {
	if err := service.DeleteAllTaskExecutions(taskID); err != nil {
		model.WriteError(w, model.Internal(err))
		return true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return true
}

// applyTaskUpdate applies the update request fields to the task.
func applyTaskUpdate(task *model.ScheduledTask, req struct {
	Action      string `json:"action"`
	ExecutionID string `json:"executionId"`
	Name        string `json:"name"`
	CronExpr    string `json:"cron_expr"`
	AgentID     string `json:"agent_id"`
	Prompt      string `json:"prompt"`
	RepeatMode  string `json:"repeat_mode"`
	MaxRuns     *int   `json:"max_runs"`
},
) {
	if req.Name != "" {
		task.Name = req.Name
	}
	if req.CronExpr != "" {
		task.CronExpr = req.CronExpr
	}
	if req.AgentID != "" {
		task.AgentID = req.AgentID
	}
	if req.Prompt != "" {
		task.Prompt = req.Prompt
	}
	if req.RepeatMode != "" {
		task.RepeatMode = req.RepeatMode
	}
	if req.MaxRuns != nil {
		task.MaxRuns = *req.MaxRuns
	}
	if task.Status == "completed" {
		task.Status = "active"
		task.RunCount = 0
	}
}

func serveTaskByIDDelete(w http.ResponseWriter, r *http.Request, taskID int64, projectPath string) {
	task, err := service.GetTaskByID(taskID)
	if err != nil {
		writeLocalizedError(w, r, model.NotFound(nil, "TaskNotFound"))
		return
	}
	if task.ProjectPath != projectPath {
		writeLocalizedError(w, r, model.Forbidden(nil, "AccessDenied"))
		return
	}
	service.GlobalScheduler.CancelAllExecutions(taskID)
	service.GlobalScheduler.RemoveTask(taskID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// serveTaskExecutions returns the execution history for a task.
// It joins task_executions with chat_history to fetch the assistant content.
// Supports cursor-based pagination: ?limit=N&cursor=timestamp&cursor_id=id
// When limit > 0, returns { executions, hasMore }. Otherwise returns all (no hasMore).
func serveTaskExecutions(w http.ResponseWriter, r *http.Request, taskID int64, projectPath string) {
	task, err := service.GetTaskByID(taskID)
	if err != nil {
		writeLocalizedError(w, r, model.NotFound(nil, "TaskNotFound"))
		return
	}
	if task.ProjectPath != projectPath {
		writeLocalizedError(w, r, model.Forbidden(nil, "AccessDenied"))
		return
	}

	limit, cursor, cursorID := parseExecPagination(r)
	query, args := buildExecQuery(taskID, limit, cursor, cursorID)

	rows, err := service.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		model.WriteError(w, model.Internal(fmt.Errorf("failed to load execution history")))
		return
	}
	defer func() { _ = rows.Close() }()

	executions := scanExecRows(rows, task)
	if err := rows.Err(); err != nil {
		model.WriteError(w, model.Internal(fmt.Errorf("failed to iterate execution records")))
		return
	}

	if executions == nil {
		executions = []Execution{}
	}

	hasMore := false
	if limit > 0 && len(executions) > limit {
		hasMore = true
		executions = executions[:limit]
	}

	result := map[string]any{"executions": executions}
	if limit > 0 {
		result["hasMore"] = hasMore
	}
	writeJSON(w, http.StatusOK, result)
}

// Execution represents a task execution in API responses.
type Execution struct {
	ID          int64   `json:"id"`
	SessionID   string  `json:"sessionId"`
	TriggerType string  `json:"triggerType"`
	Status      string  `json:"status"`
	Content     *string `json:"content"`
	Summary     *string `json:"summary"`
	CreatedAt   string  `json:"createdAt"`
	IsUnread    bool    `json:"isUnread"`
}

func parseExecPagination(r *http.Request) (limit int, cursor, cursorID string) {
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, atoiErr := strconv.Atoi(limitStr); atoiErr == nil && l > 0 {
			limit = l
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

func buildExecQuery(taskID int64, limit int, cursor, cursorID string) (query string, args []any) {
	query = `
		SELECT te.id, te.session_id, te.trigger_type, te.status, te.created_at,
		       te.read_at, sm.summary,
		       ch.content AS assistant_content
		FROM task_executions te
		LEFT JOIN summaries sm ON sm.target_type = 'task_execution' AND sm.target_id = te.id
		LEFT JOIN chat_history ch ON ch.session_id = te.session_id
		    AND ch.role = 'assistant'
		    AND ch.deleted = 0
		    AND ch.streaming = 0
		WHERE te.task_id = ?`
	args = []any{taskID}

	if cursor != "" && cursorID != "" {
		cursorIDInt, cerr := strconv.ParseInt(cursorID, 10, 64)
		if cerr == nil && cursorIDInt > 0 {
			query += " AND (te.created_at < ? OR (te.created_at = ? AND te.id < ?))"
			args = append(args, cursor, cursor, cursorIDInt)
		}
	}

	query += " ORDER BY te.created_at DESC, te.id DESC"

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit+1)
	}
	return query, args
}

func scanExecRows(rows *sql.Rows, task *model.ScheduledTask) []Execution {
	var executions []Execution
	for rows.Next() {
		var exec Execution
		var content sql.NullString
		var summary sql.NullString
		var readAt sql.NullTime
		if err := rows.Scan(&exec.ID, &exec.SessionID, &exec.TriggerType, &exec.Status, &exec.CreatedAt, &readAt, &summary, &content); err != nil {
			break
		}
		if content.Valid {
			exec.Content = &content.String
		}
		if summary.Valid {
			exec.Summary = &summary.String
		}
		exec.IsUnread = isExecUnread(readAt, exec.Status, exec.CreatedAt, task)
		executions = append(executions, exec)
	}
	return executions
}

func isExecUnread(readAt sql.NullTime, status, createdAt string, task *model.ScheduledTask) bool {
	if readAt.Valid || status == jsonKeyRunning {
		return false
	}
	if task.LastReadAt == nil {
		return true
	}
	createdAtTime, parseErr := time.Parse(time.RFC3339, createdAt)
	if parseErr == nil {
		return createdAtTime.After(*task.LastReadAt)
	}
	return false
}
