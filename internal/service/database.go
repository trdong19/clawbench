// Package service provides the core business logic for ClawBench's backend,
// including chat persistence, scheduling, proxy management, and database operations.
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"clawbench/internal/model"

	// modernc.org/sqlite: SQLite driver registration required for database/sql.Open("sqlite", ...)
	_ "modernc.org/sqlite"
)

// DB is the write database connection pool (MaxOpenConns=1) for INSERT/UPDATE/DELETE operations.
var DB *sql.DB

// DBRead is the read-only connection pool (MaxOpenConns=2) for SELECT queries.
// In WAL mode, reads never block writes and vice versa.
var DBRead *sql.DB

// runSchemaMigrations applies column additions and table migrations for older databases.
func runSchemaMigrations() error {
	migrations := []struct {
		table  string
		column string
		alter  string
	}{
		{"task_executions", "read_at", "ALTER TABLE task_executions ADD COLUMN read_at DATETIME"},
		{"task_executions", "summary", "ALTER TABLE task_executions ADD COLUMN summary TEXT"},
		{TableChatSessions, "thinking_effort", "ALTER TABLE " + TableChatSessions + " ADD COLUMN thinking_effort TEXT DEFAULT ''"},
		{"forwarded_ports", "host", "ALTER TABLE forwarded_ports ADD COLUMN host TEXT NOT NULL DEFAULT ''"},
	}
	for _, m := range migrations {
		var count int
		if err := DB.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM pragma_table_info('"+m.table+"') WHERE name=?", m.column,
		).Scan(&count); err != nil {
			return fmt.Errorf("failed to check %s.%s column: %w", m.table, m.column, err)
		}
		if count == 0 {
			if _, err := DB.ExecContext(context.Background(), m.alter); err != nil {
				return fmt.Errorf("failed to add %s.%s column: %w", m.table, m.column, err)
			}
		}
	}

	// local_port migration requires backfill
	var hasLocalPort int
	if err := DB.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM pragma_table_info('forwarded_ports') WHERE name='local_port'",
	).Scan(&hasLocalPort); err != nil {
		return fmt.Errorf("failed to check local_port column: %w", err)
	}
	if hasLocalPort == 0 {
		if _, err := DB.ExecContext(context.Background(), "ALTER TABLE forwarded_ports ADD COLUMN local_port INTEGER"); err != nil {
			return fmt.Errorf("failed to add local_port column: %w", err)
		}
		if _, err := DB.ExecContext(context.Background(), "UPDATE forwarded_ports SET local_port = port WHERE local_port IS NULL"); err != nil {
			return fmt.Errorf("failed to backfill local_port: %w", err)
		}
	}

	// TTS summaries migration: cache_key → message_id
	var hasTTSCacheKey int
	if err := DB.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM pragma_table_info('tts_summaries') WHERE name='cache_key'",
	).Scan(&hasTTSCacheKey); err != nil {
		return fmt.Errorf("failed to check tts_summaries cache_key: %w", err)
	}
	if hasTTSCacheKey > 0 {
		if _, err := DB.ExecContext(context.Background(), "DROP TABLE tts_summaries"); err != nil {
			return fmt.Errorf("failed to drop old tts_summaries table: %w", err)
		}
	}
	var hasTTSSummaries int
	if err := DB.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tts_summaries'",
	).Scan(&hasTTSSummaries); err != nil {
		return fmt.Errorf("failed to check tts_summaries table: %w", err)
	}
	if hasTTSSummaries == 0 {
		if _, err := DB.ExecContext(context.Background(), `
			CREATE TABLE tts_summaries (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				message_id   INTEGER NOT NULL,
				tts_summary  TEXT NOT NULL,
				created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
				UNIQUE(message_id)
			);
		`); err != nil {
			return fmt.Errorf("failed to create tts_summaries table: %w", err)
		}
	}
	return nil
}

// cleanupOrphanedStreaming finalizes orphaned streaming messages from previous crashes.
func cleanupOrphanedStreaming() {
	rows, err := DB.QueryContext(context.Background(), "SELECT id, content FROM chat_history WHERE streaming = 1")
	if err != nil {
		slog.Error("failed to query orphaned streaming messages", slog.String("err", err.Error()))
		return
	}
	defer func() { _ = rows.Close() }()
	type orphanMsg struct {
		id      int64
		content string
	}
	var orphans []orphanMsg
	for rows.Next() {
		var m orphanMsg
		if err := rows.Scan(&m.id, &m.content); err != nil {
			slog.Error("failed to scan orphaned streaming message", slog.String("err", err.Error()))
			return
		}
		orphans = append(orphans, m)
	}
	if err := rows.Err(); err != nil {
		slog.Error("failed to iterate orphaned streaming messages", slog.String("err", err.Error()))
		return
	}

	for _, m := range orphans {
		var contentMap map[string]any
		if err := json.Unmarshal([]byte(m.content), &contentMap); err != nil {
			contentMap = map[string]any{
				BlockKeyBlocks:   []any{map[string]any{JSONKeyType: BlockTypeText, BlockTypeText: m.content}},
				JSONKeyCancelled: true,
			}
		} else {
			contentMap[JSONKeyCancelled] = true
			blocks, _ := contentMap[BlockKeyBlocks].([]any)
			blocks = append(blocks, map[string]any{
				JSONKeyType:   BlockTypeWarning,
				BlockTypeText: WarningServerRestarted,
				JSONKeyReason: JSONValueRestart,
			})
			contentMap[BlockKeyBlocks] = blocks
		}
		updatedContent, _ := json.Marshal(contentMap)
		if _, err := DB.ExecContext(context.Background(), "UPDATE chat_history SET content = ?, streaming = 0 WHERE id = ?", string(updatedContent), m.id); err != nil {
			slog.Error("failed to finalize orphaned streaming message", slog.Int64("id", m.id), slog.String("err", err.Error()))
		}
	}
	if len(orphans) > 0 {
		slog.Info("cleaned up orphaned streaming messages", slog.Int("count", len(orphans)))
	}
}

// InitDB initializes the SQLite database with latest schema.
// When runFromServer is true (server startup), orphaned streaming messages
// from previous crashes are cleaned up. When false (CLI subcommand), cleanup
// is skipped because the server process may still be actively streaming.
func InitDB(runFromServer ...bool) error {
	dbDir := filepath.Join(model.BinDir, ".clawbench")
	if err := os.MkdirAll(dbDir, 0o750); err != nil {
		return fmt.Errorf("failed to create db directory: %w", err)
	}

	dbPath := filepath.Join(dbDir, "ClawBench.db")
	var err error
	DB, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// SQLite concurrency: single connection + WAL mode + busy timeout
	DB.SetMaxOpenConns(1)

	// Enable WAL mode for concurrent reads during writes
	if _, walErr := DB.ExecContext(context.Background(), "PRAGMA journal_mode=WAL"); walErr != nil {
		return fmt.Errorf("failed to set WAL mode: %w", walErr)
	}
	// Wait up to 5 seconds when database is locked instead of failing immediately
	if _, btErr := DB.ExecContext(context.Background(), "PRAGMA busy_timeout=5000"); btErr != nil {
		return fmt.Errorf("failed to set busy_timeout: %w", btErr)
	}

	// Create tables with latest schema
	_, err = DB.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS chat_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_path TEXT NOT NULL,
			role TEXT NOT NULL CHECK(role IN ('user', 'assistant')),
			content TEXT NOT NULL,
			files TEXT,
			session_id TEXT,
			backend TEXT NOT NULL DEFAULT 'claude',
			streaming INTEGER NOT NULL DEFAULT 0,
			indexed INTEGER NOT NULL DEFAULT 0,
			deleted INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS chat_sessions (
			id TEXT PRIMARY KEY,
			project_path TEXT NOT NULL,
			backend TEXT NOT NULL,
			title TEXT NOT NULL,
			agent_id TEXT DEFAULT '',
			agent_source TEXT DEFAULT 'default',
			model TEXT DEFAULT '',
			external_session_id TEXT DEFAULT '',
			session_type TEXT NOT NULL DEFAULT 'chat',
			deleted INTEGER NOT NULL DEFAULT 0,
			last_read_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(project_path, backend, id)
		);
		CREATE TABLE IF NOT EXISTS recent_projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_path TEXT UNIQUE NOT NULL,
			accessed_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS scheduled_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_path TEXT NOT NULL,
			name TEXT NOT NULL,
			cron_expr TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			prompt TEXT NOT NULL,
			session_id TEXT DEFAULT '',
			status TEXT DEFAULT 'active',
			repeat_mode TEXT DEFAULT 'unlimited',
			max_runs INTEGER DEFAULT 0,
			last_run_at DATETIME,
			next_run_at DATETIME,
			run_count INTEGER DEFAULT 0,
			last_read_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS task_executions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id INTEGER NOT NULL,
			session_id TEXT NOT NULL,
			trigger_type TEXT NOT NULL DEFAULT 'auto',
			status TEXT NOT NULL DEFAULT 'running',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS ai_raw_responses (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			message_id INTEGER NOT NULL REFERENCES chat_history(id),
			backend TEXT NOT NULL DEFAULT '',
			raw_output TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		-- Create indexes for efficient queries
		CREATE INDEX IF NOT EXISTS idx_executions_task ON task_executions(task_id, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_history_session ON chat_history(project_path, backend, session_id, created_at);
		CREATE INDEX IF NOT EXISTS idx_sessions_project_backend ON chat_sessions(project_path, backend);
		CREATE INDEX IF NOT EXISTS idx_raw_responses_session ON ai_raw_responses(session_id, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_raw_responses_message ON ai_raw_responses(message_id);
		CREATE INDEX IF NOT EXISTS idx_executions_session ON task_executions(session_id);
		CREATE INDEX IF NOT EXISTS idx_sessions_type ON chat_sessions(session_type, project_path, deleted);

		-- Covering index for session-based queries (GetChatMessageCount, GetAssistantMessageCount,
		-- unread subquery, GetChatHistoryPaged) — avoids full table scan through large content rows.
		CREATE INDEX IF NOT EXISTS idx_history_session_id ON chat_history(session_id, deleted, role, streaming, created_at);
		-- Index for task listing by project
		CREATE INDEX IF NOT EXISTS idx_tasks_project ON scheduled_tasks(project_path, created_at DESC);

		CREATE TABLE IF NOT EXISTS summaries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			target_type TEXT NOT NULL,
			target_id   INTEGER NOT NULL,
			summary     TEXT NOT NULL,
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(target_type, target_id)
		);

		CREATE TABLE IF NOT EXISTS forwarded_ports (
			local_port INTEGER PRIMARY KEY,
			port INTEGER NOT NULL,
			host TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			protocol TEXT NOT NULL DEFAULT 'http',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS terminal_quick_commands (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			label TEXT NOT NULL,
			command TEXT NOT NULL,
			hidden INTEGER NOT NULL DEFAULT 0,
			auto_execute INTEGER NOT NULL DEFAULT 0,
			sort_order INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_quick_commands_auto_execute
			ON terminal_quick_commands(auto_execute) WHERE auto_execute = 1;

		CREATE TABLE IF NOT EXISTS chat_quick_send (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			label TEXT NOT NULL,
			command TEXT NOT NULL,
			sort_order INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}

	// Schema migrations: add columns that may not exist in older databases.
	if migrateErr := runSchemaMigrations(); migrateErr != nil {
		return migrateErr
	}

	// Clean up orphaned streaming messages from previous crashes/restarts.
	// Any message with streaming=1 at startup can never be finalized since
	// its stream no longer exists. Mark them as cancelled so the UI shows
	// an interrupted state instead of silently completing.
	// SKIP when called from CLI subcommands (task/rag) — the server process
	// may still be actively streaming, and these are NOT orphaned messages.
	isServerStartup := len(runFromServer) > 0 && runFromServer[0]

	// Initialize read connection pool for concurrent reads (WAL mode).
	// WAL contract: DB (MaxOpenConns=1) serializes writes; DBRead (MaxOpenConns=2)
	// allows concurrent reads that never block writes and vice versa.
	// Both pools must use WAL mode + busy_timeout for this to work correctly.
	DBRead, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open read database: %w", err)
	}
	DBRead.SetMaxOpenConns(2)
	DBRead.SetMaxIdleConns(2)                   // match MaxOpenConns to avoid churn
	DBRead.SetConnMaxLifetime(0)                // unlimited — SQLite file DB, no reconnection needed
	DBRead.SetConnMaxIdleTime(30 * time.Minute) // close idle conns after 30min
	if _, err := DBRead.ExecContext(context.Background(), "PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("failed to set read DB WAL mode: %w", err)
	}
	if _, err := DBRead.ExecContext(context.Background(), "PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("failed to set read DB busy_timeout: %w", err)
	}
	if isServerStartup {
		cleanupOrphanedStreaming()
	}

	return nil
}

// CloseDB closes both write and read database connections.
func CloseDB() {
	if DB != nil {
		_ = DB.Close()
	}
	if DBRead != nil {
		_ = DBRead.Close()
	}
}

// GetSummary looks up a reading summary by target type and target ID.
// Returns (summary, found). Empty summary = text was too short.
func GetSummary(targetType string, targetID int64) (string, bool) {
	var summary string
	err := DBRead.QueryRowContext(
		context.Background(),
		"SELECT summary FROM summaries WHERE target_type = ? AND target_id = ?",
		targetType, targetID,
	).Scan(&summary)
	if err != nil {
		return "", false
	}
	return summary, true
}

// SaveSummary persists a reading summary for a target (chat message or task execution).
// summary = "" means text was too short; non-empty is the actual summary.
func SaveSummary(targetType string, targetID int64, summary string) error {
	_, err := DB.ExecContext(
		context.Background(),
		"INSERT OR REPLACE INTO summaries (target_type, target_id, summary, created_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)",
		targetType, targetID, summary,
	)
	return err
}

// GetTTSSummaryByMessageID looks up a TTS summary by message ID.
// Returns (ttsSummary, found).
func GetTTSSummaryByMessageID(messageID int64) (string, bool) {
	var ttsSummary string
	err := DBRead.QueryRowContext(
		context.Background(),
		"SELECT tts_summary FROM tts_summaries WHERE message_id = ?",
		messageID,
	).Scan(&ttsSummary)
	if err != nil {
		return "", false
	}
	return ttsSummary, true
}

// SaveTTSSummaryByMessageID persists a TTS summary for a chat message.
func SaveTTSSummaryByMessageID(messageID int64, ttsSummary string) error {
	_, err := DB.ExecContext(
		context.Background(),
		"INSERT OR REPLACE INTO tts_summaries (message_id, tts_summary, created_at) VALUES (?, ?, CURRENT_TIMESTAMP)",
		messageID, ttsSummary,
	)
	return err
}

// quickCommandExtra holds the additional fields needed for terminal_quick_commands
// beyond the shared (label, command, sort_order) triplet.
type quickCommandExtra struct{ hidden, autoExec int }

// QuickCommandHelpers exposes the shared CRUD helpers for terminal_quick_commands.
var QuickCommandHelpers = crudHelpers[QuickCommand, quickCommandExtra]{
	table:     "terminal_quick_commands",
	scanCols:  "id, label, command, hidden, auto_execute, sort_order",
	insertSQL: "INSERT INTO terminal_quick_commands (label, command, hidden, auto_execute, sort_order) VALUES (?, ?, ?, ?, ?)",
	updateSQL: "UPDATE terminal_quick_commands SET label = ?, command = ?, hidden = ?, auto_execute = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
	scanFn: func(rows *sql.Rows) (QuickCommand, error) {
		var cmd QuickCommand
		var hidden, autoExec int
		if err := rows.Scan(&cmd.ID, &cmd.Label, &cmd.Command, &hidden, &autoExec, &cmd.SortOrder); err != nil {
			return cmd, err
		}
		cmd.Hidden = hidden == 1
		cmd.AutoExecute = autoExec == 1
		return cmd, nil
	},
	addFn: func(cmd QuickCommand) (label string, command string, sortOrder int, extra quickCommandExtra) {
		hidden := 0
		if cmd.Hidden {
			hidden = 1
		}
		autoExec := 0
		if cmd.AutoExecute {
			autoExec = 1
		}
		return cmd.Label, cmd.Command, cmd.SortOrder, quickCommandExtra{hidden: hidden, autoExec: autoExec}
	},
}

// ChatQuickSendHelpers exposes the shared CRUD helpers for chat_quick_send.
var ChatQuickSendHelpers = crudHelpers[ChatQuickSendItem, struct{}]{
	table:     "chat_quick_send",
	scanCols:  "id, label, command, sort_order",
	insertSQL: "INSERT INTO chat_quick_send (label, command, sort_order) VALUES (?, ?, ?)",
	updateSQL: "UPDATE chat_quick_send SET label = ?, command = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
	scanFn: func(rows *sql.Rows) (ChatQuickSendItem, error) {
		var item ChatQuickSendItem
		return item, rows.Scan(&item.ID, &item.Label, &item.Command, &item.SortOrder)
	},
	addFn: func(item ChatQuickSendItem) (label string, command string, sortOrder int, _ struct{}) {
		return item.Label, item.Command, item.SortOrder, struct{}{}
	},
}

// crudHelpers[T, E] holds the table-specific operations needed for CRUD on typed struct [T].
// E carries table-specific extra data for Insert/Update beyond (label, command, sortOrder).
type crudHelpers[T any, E any] struct {
	table     string
	scanCols  string // columns for SELECT (must match field order in scanFn)
	scanFn    func(*sql.Rows) (T, error)
	addFn     func(T) (label string, command string, sortOrder int, extra E)
	insertSQL string
	updateSQL string
}

// list returns all rows from the helper's table ordered by sort_order.
func (h crudHelpers[T, E]) list() ([]T, error) {
	rows, err := DBRead.QueryContext(context.Background(), "SELECT "+h.scanCols+" FROM "+h.table+" ORDER BY sort_order") //nolint:gosec // table/column names are constants, not user input
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var items []T
	for rows.Next() {
		item, err := h.scanFn(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// insert adds a new row. For tables with an auto_execute column (E=quickCommandExtra),
// any existing auto_execute=1 rows are cleared first to enforce the single-active invariant.
func (h crudHelpers[T, E]) insert(item T) (int64, error) {
	// Capture addFn result so we can inspect extra (for auto_execute check)
	// without calling the closure twice.
	label, command, sortOrder, extra := h.addFn(item)
	if _, ok := any(extra).(quickCommandExtra); ok {
		if _, err := DB.ExecContext(context.Background(), "UPDATE "+h.table+" SET auto_execute = 0 WHERE auto_execute = 1"); err != nil { //nolint:gosec // table/column names are constants, not user input
			return 0, err
		}
	}
	var maxOrder sql.NullInt64
	_ = DB.QueryRowContext(context.Background(), "SELECT MAX(sort_order) FROM "+h.table).Scan(&maxOrder)
	if maxOrder.Valid {
		sortOrder = int(maxOrder.Int64) + 1
	}
	var args []any
	if e, ok := any(extra).(quickCommandExtra); ok {
		args = []any{label, command, sortOrder, e.autoExec, e.hidden}
	} else {
		args = []any{label, command, sortOrder}
	}
	result, err := DB.ExecContext(context.Background(), h.insertSQL, args...)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// update modifies an existing row by id. For tables with an auto_execute column,
// clears auto_execute on other rows to enforce the single-active invariant.
func (h crudHelpers[T, E]) update(id int64, item T) error {
	label, command, _, extra := h.addFn(item)
	if _, ok := any(extra).(quickCommandExtra); ok {
		if _, err := DB.ExecContext(context.Background(), "UPDATE "+h.table+" SET auto_execute = 0 WHERE auto_execute = 1 AND id != ?", id); err != nil { //nolint:gosec // table/column names are constants, not user input
			return err
		}
	}
	var args []any
	if e, ok := any(extra).(quickCommandExtra); ok {
		args = []any{label, command, e.autoExec, e.hidden, id}
	} else {
		args = []any{label, command, id}
	}
	_, err := DB.ExecContext(context.Background(), h.updateSQL, args...)
	return err
}

// delete removes a row by id.
func (h crudHelpers[T, E]) delete(id int64) error {
	_, err := DB.ExecContext(context.Background(), "DELETE FROM "+h.table+" WHERE id = ?", id) //nolint:gosec // table/column names are constants, not user input
	return err
}

// reorder updates sort_order for all rows matching the given id list.
func (h crudHelpers[T, E]) reorder(ids []int64) error {
	tx, err := DB.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(context.Background(), "UPDATE "+h.table+" SET sort_order = ? WHERE id = ?", i, id); err != nil { //nolint:gosec // table/column names are constants, not user input
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// QuickCommand represents a terminal quick command stored in the database.
type QuickCommand struct {
	ID          int64  `json:"id"`
	Label       string `json:"label"`
	Command     string `json:"command"`
	Hidden      bool   `json:"hidden"`
	AutoExecute bool   `json:"auto_execute"`
	SortOrder   int    `json:"sort_order"`
}

// GetQuickCommands returns all quick commands ordered by sort_order.
func GetQuickCommands() ([]QuickCommand, error) {
	return QuickCommandHelpers.list()
}

// AddQuickCommand inserts a new quick command and returns its ID.
// If autoExecute is true, other commands' auto_execute flag is cleared first.
func AddQuickCommand(label, command string, hidden, autoExecute bool) (int64, error) {
	return QuickCommandHelpers.insert(QuickCommand{Label: label, Command: command, Hidden: hidden, AutoExecute: autoExecute})
}

// UpdateQuickCommand updates an existing quick command.
// If autoExecute is true, other commands' auto_execute flag is cleared first.
func UpdateQuickCommand(id int64, label, command string, hidden, autoExecute bool) error {
	return QuickCommandHelpers.update(id, QuickCommand{Label: label, Command: command, Hidden: hidden, AutoExecute: autoExecute})
}

// DeleteQuickCommand deletes a quick command by ID.
func DeleteQuickCommand(id int64) error {
	return QuickCommandHelpers.delete(id)
}

// ReorderQuickCommands updates sort_order for all commands based on the given ID order.
func ReorderQuickCommands(ids []int64) error {
	return QuickCommandHelpers.reorder(ids)
}

// ChatQuickSendItem represents a chat quick-send item stored in the database.
type ChatQuickSendItem struct {
	ID        int64  `json:"id"`
	Label     string `json:"label"`
	Command   string `json:"command"`
	SortOrder int    `json:"sort_order"`
}

// GetChatQuickSend returns all quick-send items ordered by sort_order.
func GetChatQuickSend() ([]ChatQuickSendItem, error) {
	return ChatQuickSendHelpers.list()
}

// AddChatQuickSend inserts a new quick-send item and returns its ID.
func AddChatQuickSend(label, command string) (int64, error) {
	return ChatQuickSendHelpers.insert(ChatQuickSendItem{Label: label, Command: command})
}

// UpdateChatQuickSend updates an existing quick-send item.
func UpdateChatQuickSend(id int64, label, command string) error {
	return ChatQuickSendHelpers.update(id, ChatQuickSendItem{Label: label, Command: command})
}

// DeleteChatQuickSend deletes a quick-send item by ID.
func DeleteChatQuickSend(id int64) error {
	return ChatQuickSendHelpers.delete(id)
}

// ReorderChatQuickSend updates sort_order for all items based on the given ID order.
func ReorderChatQuickSend(ids []int64) error {
	return ChatQuickSendHelpers.reorder(ids)
}
