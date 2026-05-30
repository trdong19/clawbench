// Package service provides the core business logic for ClawBench's backend,
// including chat persistence, scheduling, proxy management, and database operations.
package service

// Role constants for chat messages.
const (
	RoleAssistant = "assistant"
	RoleUser      = "user"
)

// Content block type constants.
const (
	BlockTypeText     = "text"
	BlockTypeToolUse  = "tool_use"
	BlockTypeWarning  = "warning"
	BlockTypeThinking = "thinking"
	BlockKeyBlocks    = "blocks" // JSON key for content blocks
)

// TableChatSessions is the database table name for chat sessions.
const TableChatSessions = "chat_sessions"

// Task status constants.
const (
	StatusActive    = "active"
	StatusPaused    = "paused"
	StatusCompleted = "completed"
	StatusRunning   = "running"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Trigger type constants.
const (
	TriggerAuto   = "auto"
	TriggerManual = "manual"
)

// Proxy-related constants.
const (
	DefaultAllowedPorts     = "1024-65535"
	DefaultAllowedPortsHTTP = "1024-5000,8080"
	LocalhostIP             = "127.0.0.1"
	ProxyCategoryOther      = "other"
	ProtocolHTTP            = "http"
	ProtocolHTTPS           = "https"
	ProxyLocalhost          = "localhost"
)

// Stream event type constants.
const (
	StreamTypeContent = "content"
	StreamTypeDone    = "done"
)

// JSONKeyType is the JSON key constant for block type discriminators in content maps.
const JSONKeyType = "type"

// TTS event type constants.
const (
	TTSEventTypePhase  = "phase"
	TTSEventTypeResult = "result"
)

// File watch event type constants.
const (
	WatchEventFileChange = "file_change"
	WatchEventDirChange  = "dir_change"
)

// EventType is the event type constant for scheduler/session events.
const EventType = "event"

// JSON key constants.
const (
	JSONKeyReason    = "reason"
	JSONKeyCancelled = "cancelled"
	JSONValueRestart = "restart"
)

// Tool name constants (used in ContentBlock.Name).
const (
	ToolNameRead = "Read"
	ToolNameBash = "Bash"
)

// TestPathPrefix is the test path constant for filewatch tests.
const TestPathPrefix = "/test"

// TestDebounceKeyC1Dir is the test debounce key constant for filewatch tests.
const TestDebounceKeyC1Dir = "c1|dir_change"

// Test execution ID constants for scheduler tests.
const (
	TestExecID1 = "exec-1"
	TestExecID2 = "exec-2"
	TestExecID3 = "exec-3"
)

// TestHello is the test content constant for test data.
const TestHello = "hello"

// TestToolID1 is a test tool use ID.
const TestToolID1 = "tool-1"

// WarningServerRestarted is the warning text for orphaned streaming messages after server restart.
const WarningServerRestarted = "Server restarted, AI response interrupted"
