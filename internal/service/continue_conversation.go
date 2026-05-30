package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"clawbench/internal/model"
)

// CheckContinueSession checks whether a continued chat session already exists
// for the given task execution. Returns (exists, sessionID, error).
func CheckContinueSession(ctx context.Context, execID int64) (exists bool, sessionID string, err error) {
	var sourceSessionID string
	err = DBRead.QueryRowContext(ctx, "SELECT session_id FROM task_executions WHERE id = ?", execID).Scan(&sourceSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", fmt.Errorf("execution %d not found", execID)
	}
	if err != nil {
		return false, "", err
	}

	var existingID string
	err = DBRead.QueryRowContext(
		ctx,
		"SELECT id FROM chat_sessions WHERE source_session_id = ? AND session_type = 'chat' AND deleted = 0",
		sourceSessionID,
	).Scan(&existingID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, existingID, nil
}

// sourceSessionMeta holds metadata from the source session needed for continuation.
type sourceSessionMeta struct {
	backend        string
	agentID        string
	agentSource    string
	modelName      string
	thinkingEffort string
	projectPath    string
}

// ContinueFromExecution creates a new chat session from a scheduled task execution,
// copying the original session's chat_history and summaries. If a continued session
// already exists (and is not deleted), it returns the existing session ID with
// alreadyExists=true.
//
// In production, DB has MaxOpenConns=1 so all writes are serialized through a single
// connection — this provides the same atomicity guarantee as BEGIN IMMEDIATE without
// the risk of connection-pool deadlocks in test environments.
func ContinueFromExecution(ctx context.Context, execID int64, projectPath string) (sessionID string, alreadyExists bool, err error) {
	sourceSessionID, taskID, execStatus, err := getExecutionInfo(ctx, execID)
	if err != nil {
		return "", false, err
	}

	if execStatus == "running" {
		return "", false, fmt.Errorf("execution %d is still running", execID)
	}

	taskName, taskProjectPath, err := getTaskInfo(ctx, taskID)
	if err != nil {
		return "", false, err
	}

	if taskProjectPath != projectPath {
		return "", false, fmt.Errorf("execution %d does not belong to project %q", execID, projectPath)
	}

	meta, err := getSourceSessionMeta(ctx, sourceSessionID)
	if err != nil {
		return "", false, err
	}

	existingID, err := findExistingContinuedSession(ctx, sourceSessionID)
	if err != nil {
		return "", false, err
	}
	if existingID != "" {
		return existingID, true, nil
	}

	if err = checkSessionLimit(ctx, meta.projectPath); err != nil { //nolint:gocritic // named return: = would shadow
		return "", false, err
	}

	newSessionID := generateSessionID()
	if _, err = DB.ExecContext(
		ctx,
		"INSERT INTO chat_sessions (id, project_path, backend, title, agent_id, agent_source, model, session_type, source_session_id, thinking_effort) VALUES (?, ?, ?, ?, ?, ?, ?, 'chat', ?, ?)",
		newSessionID, meta.projectPath, meta.backend, taskName, meta.agentID, meta.agentSource, meta.modelName, sourceSessionID, meta.thinkingEffort,
	); err != nil {
		return "", false, fmt.Errorf("failed to create continued session: %w", err)
	}

	idMap, err := copyChatHistory(ctx, sourceSessionID, newSessionID)
	if err != nil {
		return "", false, err
	}

	if err = copySummaries(ctx, idMap); err != nil { //nolint:gocritic // named return: = would shadow
		return "", false, err
	}

	return newSessionID, false, nil
}

func getExecutionInfo(ctx context.Context, execID int64) (sourceSessionID string, taskID int64, status string, err error) {
	err = DB.QueryRowContext(
		ctx,
		"SELECT session_id, task_id, status FROM task_executions WHERE id = ?",
		execID,
	).Scan(&sourceSessionID, &taskID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, "", fmt.Errorf("execution %d not found", execID)
	}
	return sourceSessionID, taskID, status, err
}

func getTaskInfo(ctx context.Context, taskID int64) (name, projectPath string, err error) {
	err = DB.QueryRowContext(
		ctx,
		"SELECT name, project_path FROM scheduled_tasks WHERE id = ?",
		taskID,
	).Scan(&name, &projectPath)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("task %d not found", taskID)
	}
	return name, projectPath, err
}

func getSourceSessionMeta(ctx context.Context, sourceSessionID string) (sourceSessionMeta, error) {
	var m sourceSessionMeta
	err := DB.QueryRowContext(
		ctx,
		"SELECT backend, agent_id, agent_source, model, thinking_effort, project_path FROM chat_sessions WHERE id = ?",
		sourceSessionID,
	).Scan(&m.backend, &m.agentID, &m.agentSource, &m.modelName, &m.thinkingEffort, &m.projectPath)
	if errors.Is(err, sql.ErrNoRows) {
		return sourceSessionMeta{}, fmt.Errorf("source session %s not found", sourceSessionID)
	}
	return m, err
}

func findExistingContinuedSession(ctx context.Context, sourceSessionID string) (string, error) {
	var existingID string
	err := DB.QueryRowContext(
		ctx,
		"SELECT id FROM chat_sessions WHERE source_session_id = ? AND session_type = 'chat' AND deleted = 0",
		sourceSessionID,
	).Scan(&existingID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return existingID, err
}

func checkSessionLimit(ctx context.Context, projectPath string) error {
	if model.SessionMaxCount <= 0 {
		return nil
	}
	var count int
	err := DB.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM chat_sessions WHERE project_path = ? AND deleted = 0 AND session_type = 'chat'",
		projectPath,
	).Scan(&count)
	if err != nil {
		return err
	}
	if count >= model.SessionMaxCount {
		return fmt.Errorf("session limit reached (%d/%d)", count, model.SessionMaxCount)
	}
	return nil
}

type sourceMsg struct {
	id          int64
	projectPath string
	role        string
	content     string
	files       sql.NullString
	backend     string
	createdAt   sql.NullString
}

func copyChatHistory(ctx context.Context, sourceSessionID, newSessionID string) (map[int64]int64, error) {
	rows, err := DB.QueryContext(
		ctx,
		"SELECT id, project_path, role, content, files, backend, created_at FROM chat_history WHERE session_id = ? AND deleted = 0 AND streaming = 0 ORDER BY id",
		sourceSessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query source messages: %w", err)
	}
	defer closeRows(rows)

	var messages []sourceMsg
	for rows.Next() {
		var m sourceMsg
		if err := rows.Scan(&m.id, &m.projectPath, &m.role, &m.content, &m.files, &m.backend, &m.createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan source message: %w", err)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate source messages: %w", err)
	}

	idMap := make(map[int64]int64)
	for _, m := range messages {
		var createdAt interface{}
		if m.createdAt.Valid {
			createdAt = m.createdAt.String
		} else {
			createdAt = nil
		}
		result, err := DB.ExecContext(
			ctx,
			"INSERT INTO chat_history (project_path, role, content, files, session_id, backend, streaming, deleted, created_at) VALUES (?, ?, ?, ?, ?, ?, 0, 0, ?)",
			m.projectPath, m.role, m.content, m.files, newSessionID, m.backend, createdAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to copy message %d: %w", m.id, err)
		}
		newID, _ := result.LastInsertId()
		idMap[m.id] = newID
	}
	return idMap, nil
}

func copySummaries(ctx context.Context, idMap map[int64]int64) error {
	for oldID, newID := range idMap {
		var summary string
		err := DB.QueryRowContext(
			ctx,
			"SELECT summary FROM summaries WHERE target_type = 'chat_message' AND target_id = ?",
			oldID,
		).Scan(&summary)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to query summary for message %d: %w", oldID, err)
		}
		if _, err = DB.ExecContext(
			ctx,
			"INSERT OR REPLACE INTO summaries (target_type, target_id, summary, created_at) VALUES ('chat_message', ?, ?, CURRENT_TIMESTAMP)",
			newID, summary,
		); err != nil {
			return fmt.Errorf("failed to copy summary for message %d: %w", oldID, err)
		}
	}
	return nil
}

func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}
