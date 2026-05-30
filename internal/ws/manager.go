package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"clawbench/internal/i18n"
	"clawbench/internal/model"
	"clawbench/internal/push"
)

// ClientSubscription tracks a single client's WS connection and push state.
type ClientSubscription struct {
	mu          sync.Mutex
	conn        *websocket.Conn
	writeMu     *sync.Mutex // shared with EventsHandler for serialized writes
	clientID    string      // identifies the client device (for logging)
	pushRegID   string      // JPush registration ID (set via WS "register" message)
	locale      string      // user's preferred locale (for push notification i18n)
	lastActive  time.Time
	eventBuffer []ServerMessage
	bufferStart time.Time
}

// maxSubscriptions limits the number of concurrent WS subscriptions to prevent
// resource exhaustion. Matches the original SSE limit of 20.
const maxSubscriptions = 20

// pushAlertMaxRunes is an alias for model.ResponsePreviewMaxRunes for local use.
const pushAlertMaxRunes = model.ResponsePreviewMaxRunes

// wsWriteTimeout is the maximum time to wait for a WebSocket write to complete.
const wsWriteTimeout = 5 * time.Second

// disconnectedBufferWindow is the duration after disconnection during which
// events are still buffered for replay. After this window, events are dropped.
const disconnectedBufferWindow = 10 * time.Second

// maxBufferedEvents is the maximum number of events retained in the replay
// buffer for WS reconnection.
const maxBufferedEvents = 50

// staleNoPushTimeout is the duration after which a disconnected subscription
// without a push registration ID is cleaned up.
const staleNoPushTimeout = 120 * time.Second

// staleWithPushTimeout is the duration after which a subscription with a push
// registration ID but no WS connection is cleaned up.
const staleWithPushTimeout = 10 * 24 * time.Hour

// Manager manages all client subscriptions.
type Manager struct {
	mu            sync.Mutex
	subscriptions map[string]*ClientSubscription // keyed by clientID
	jpush         *push.JPushClient
}

var (
	defaultManager     *Manager
	defaultManagerOnce sync.Once
)

// SetManagerForTest sets the global manager for testing. Do not use in production.
func SetManagerForTest(m *Manager) {
	defaultManager = m
}

// NewManagerForTest creates a new Manager for testing.
func NewManagerForTest(jpushClient *push.JPushClient) *Manager {
	return &Manager{
		subscriptions: make(map[string]*ClientSubscription),
		jpush:         jpushClient,
	}
}

// InitManager initializes the global WebSocket manager singleton.
func InitManager(jpushClient *push.JPushClient) {
	defaultManagerOnce.Do(func() {
		defaultManager = &Manager{
			subscriptions: make(map[string]*ClientSubscription),
			jpush:         jpushClient,
		}
	})
}

// GetManager returns the global WebSocket manager instance.
func GetManager() *Manager {
	return defaultManager
}

// Subscribe registers a new WS connection for a client identified by clientID.
// If a subscription with the same clientID already exists, its connection is replaced.
func (m *Manager) Subscribe(conn *websocket.Conn, writeMu *sync.Mutex, clientID, locale string) *ClientSubscription {
	m.mu.Lock()

	// Check subscription limit (existing clientID reconnect is allowed)
	if _, exists := m.subscriptions[clientID]; !exists && len(m.subscriptions) >= maxSubscriptions {
		m.mu.Unlock()
		_ = conn.Close(websocket.StatusPolicyViolation, "too many subscriptions")
		slog.Warn("ws: subscription rejected, limit reached", "limit", maxSubscriptions, "client_id", clientID)
		return nil
	}

	sub, ok := m.subscriptions[clientID]
	if !ok {
		sub = &ClientSubscription{clientID: clientID}
		m.subscriptions[clientID] = sub
	}

	sub.mu.Lock()
	// Save existing connection to close after releasing locks
	oldConn := sub.conn
	sub.conn = conn
	sub.writeMu = writeMu
	sub.locale = locale
	sub.lastActive = time.Now()
	sub.eventBuffer = nil
	sub.bufferStart = time.Time{}
	sub.mu.Unlock()

	m.mu.Unlock()

	// Close old connection outside of locks to avoid blocking on slow networks
	if oldConn != nil {
		_ = oldConn.Close(websocket.StatusNormalClosure, "replaced")
	}

	slog.Info("ws: client subscribed", "client_id", clientID)
	return sub
}

// DisconnectClient handles WS disconnection for a specific clientID.
// This only detaches the connection — the subscription entry (including pushRegID)
// is preserved so that push notifications can still be delivered while the client
// is disconnected. Stale subscriptions are eventually cleaned up by CleanupStale.
func (m *Manager) DisconnectClient(clientID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sub, ok := m.subscriptions[clientID]
	if !ok {
		return
	}

	sub.mu.Lock()
	sub.conn = nil
	sub.writeMu = nil
	sub.bufferStart = time.Now() // start buffer window
	sub.mu.Unlock()

	slog.Info("ws: client disconnected (subscription preserved)", "client_id", clientID)
}

// RegisterPushID stores the JPush registration ID for a client.
// Called via WS "register" message — pushRegID is tied to the WS session.
// If another subscription already uses the same pushRegID, the old one is cleared
// (dedup: same device, later connection wins).
func (m *Manager) RegisterPushID(regID, clientID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Dedup: if another subscription already has this pushRegID, clear it.
	// Same device reconnecting — the new connection wins.
	if regID != "" {
		for id, sub := range m.subscriptions {
			if id == clientID {
				continue // skip self
			}
			sub.mu.Lock()
			if sub.pushRegID == regID {
				slog.Info("ws: deduplicating push reg ID, clearing from older client", "reg_id", regID, "old_client_id", id, "new_client_id", clientID)
				sub.pushRegID = ""
			}
			sub.mu.Unlock()
		}
	}

	// Set pushRegID on the target subscription
	sub, ok := m.subscriptions[clientID]
	if !ok {
		// Subscription doesn't exist yet — shouldn't happen since register
		// comes via WS which requires Subscribe first, but handle gracefully.
		sub = &ClientSubscription{clientID: clientID}
		m.subscriptions[clientID] = sub
	}
	sub.mu.Lock()
	sub.pushRegID = regID
	sub.mu.Unlock()

	slog.Info("ws: registered push ID", "client_id", clientID, "reg_id", regID)
}

// BroadcastEvent sends an event to all connected clients, or buffers/sends JPush.
// Events are fanned out to every subscription independently:
// - WS connected → send via WS (and buffer for replay)
// - WS disconnected + pushRegID → send JPush
// - WS disconnected, no pushRegID → buffer within 10s window only
func (m *Manager) BroadcastEvent(msg ServerMessage) {
	m.mu.Lock()
	// Snapshot subscription keys to avoid holding lock during sends
	keys := make([]string, 0, len(m.subscriptions))
	for k := range m.subscriptions {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	// Track which pushRegIDs have already been notified for this event
	// via any channel (WS or JPush). This prevents duplicate notifications
	// when the same device has multiple subscriptions (e.g., frontend WS + native WS).
	deliveredRegIDs := make(map[string]bool)

	for _, key := range keys {
		m.broadcastToSubscription(key, msg, deliveredRegIDs)
	}
}

// broadcastToSubscription handles event delivery for a single subscription.
// deliveredRegIDs tracks which pushRegIDs have already been notified for this event
// (via WS or JPush), preventing duplicate notifications when the same device has
// multiple subscriptions (e.g., frontend WS + native WS).
func (m *Manager) broadcastToSubscription(key string, msg ServerMessage, deliveredRegIDs map[string]bool) {
	m.mu.Lock()
	sub, ok := m.subscriptions[key]
	m.mu.Unlock()
	if !ok {
		return
	}

	sub.mu.Lock()
	conn := sub.conn
	writeMu := sub.writeMu
	pushRegID := sub.pushRegID

	if conn != nil && writeMu != nil {
		m.sendViaWebSocket(sub, conn, writeMu, pushRegID, key, msg, deliveredRegIDs)
		return
	}

	// Client is disconnected — check buffer window
	if sub.bufferStart.IsZero() || time.Since(sub.bufferStart) < disconnectedBufferWindow {
		sub.bufferEvent(msg)
	}

	m.sendPushIfTerminal(sub, pushRegID, key, msg, deliveredRegIDs)
}

// sendViaWebSocket delivers an event via WebSocket when the client is connected.
func (m *Manager) sendViaWebSocket(sub *ClientSubscription, conn *websocket.Conn, writeMu *sync.Mutex, pushRegID, key string, msg ServerMessage, deliveredRegIDs map[string]bool) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("ws: marshal event", "error", err, "client_id", key)
		sub.mu.Unlock()
		return
	}
	writeMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
	writeErr := conn.Write(ctx, websocket.MessageText, data)
	cancel()
	writeMu.Unlock()
	// Buffer event for reconnect replay
	sub.bufferEvent(msg)
	// If WS send succeeded and this subscription has a pushRegID,
	// mark it as delivered so we don't also send JPush to the same device
	if writeErr == nil && pushRegID != "" {
		deliveredRegIDs[pushRegID] = true
	}
	sub.mu.Unlock()
}

// shouldPushForEvent returns true if the event data represents a terminal state
// that warrants a push notification (completed/canceled/failed).
func shouldPushForEvent(data any) bool {
	switch d := data.(type) {
	case *SessionUpdateData:
		return d.Status == "completed" || d.Status == "cancelled"
	case *TaskUpdateData:
		return d.Status == "completed" || d.Status == "failed" || d.Status == "cancelled"
	}
	return false
}

// pushExtras extracts JPush extras from the event data.
func pushExtras(data any, eventType string) map[string]string {
	extras := map[string]string{"event_type": eventType}
	switch d := data.(type) {
	case *SessionUpdateData:
		extras["session_id"] = d.SessionID
		if d.ProjectPath != "" {
			extras["project_path"] = d.ProjectPath
		}
	case *TaskUpdateData:
		extras["task_id"] = d.TaskID
		extras["event_type"] = "task_update"
		if d.ExecutionID != "" {
			extras["execution_id"] = d.ExecutionID
		}
		if d.SessionID != "" {
			extras["session_id"] = d.SessionID
		}
		if d.ProjectPath != "" {
			extras["project_path"] = d.ProjectPath
		}
	}
	return extras
}

// pushTitleAndAlert builds the notification title and alert text from event data.
func pushTitleAndAlert(data any, eventType, locale string) (title, alert string) {
	loc := i18n.LocalizerForLocale(locale)
	title = i18n.T(loc, "PushTaskCompleted")
	alert = i18n.T(loc, "PushSessionEnded")
	if eventType == "task_update" {
		alert = i18n.T(loc, "PushScheduledTaskDone")
	}
	var sessionTitle, responsePreview string
	switch d := data.(type) {
	case *SessionUpdateData:
		sessionTitle = d.SessionTitle
		responsePreview = d.ResponsePreview
	case *TaskUpdateData:
		sessionTitle = d.SessionTitle
		responsePreview = d.ResponsePreview
	}
	if sessionTitle != "" {
		title = "Done:" + sessionTitle
	}
	if responsePreview != "" {
		alert = truncateForPush(responsePreview)
	}
	return title, alert
}

// sendPushIfTerminal sends a JPush notification for terminal events when the client is disconnected.
func (m *Manager) sendPushIfTerminal(sub *ClientSubscription, pushRegID, key string, msg ServerMessage, deliveredRegIDs map[string]bool) {
	shouldPush := shouldPushForEvent(msg.Data)

	if pushRegID != "" && m.jpush != nil && m.jpush.Enabled() && shouldPush {
		// Dedup: skip if this regID was already notified for this event
		if deliveredRegIDs[pushRegID] {
			slog.Debug("ws: skipping jpush, event already delivered to device", "reg_id", pushRegID, "client_id", key)
			sub.mu.Unlock()
			return
		}
		deliveredRegIDs[pushRegID] = true
		sub.mu.Unlock() // unlock before potentially slow network call
		extras := pushExtras(msg.Data, msg.Event)
		title, alert := pushTitleAndAlert(msg.Data, msg.Event, sub.locale)
		slog.Debug("ws: sending jpush notification", "event", msg.Event, "client_id", key, "reg_id", pushRegID, "title", title, "extras", extras)
		if err := m.jpush.SendNotification(pushRegID, title, alert, extras); err != nil {
			slog.Warn("ws: jpush notification failed", "error", err, "client_id", key)
		}
		return
	}

	// No push registration ID available — log for debugging
	if pushRegID == "" {
		slog.Debug("ws: client disconnected, no push reg ID — notification not delivered", "event", msg.Event, "client_id", key)
	}
	sub.mu.Unlock()
}

// GetBufferedEvents returns buffered events for replay on reconnect.
func (s *ClientSubscription) GetBufferedEvents() []ServerMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]ServerMessage, len(s.eventBuffer))
	copy(result, s.eventBuffer)
	return result
}

// bufferEvent appends an event to the replay buffer, keeping at most maxBufferedEvents events.
func (s *ClientSubscription) bufferEvent(msg ServerMessage) {
	s.eventBuffer = append(s.eventBuffer, msg)
	if len(s.eventBuffer) > maxBufferedEvents {
		s.eventBuffer = s.eventBuffer[len(s.eventBuffer)-maxBufferedEvents:]
	}
}

// CleanupStale removes stale subscriptions:
//   - No pushRegID + disconnected for > staleNoPushTimeout → remove
//   - Has pushRegID + no WS connection in the last staleWithPushTimeout → remove
//   - Connected subscriptions are never cleaned up.
func (m *Manager) CleanupStale() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, sub := range m.subscriptions {
		sub.mu.Lock()
		// Never clean up active connections
		if sub.conn != nil {
			sub.mu.Unlock()
			continue
		}
		// Must have been disconnected (bufferStart is set)
		if sub.bufferStart.IsZero() {
			sub.mu.Unlock()
			continue
		}
		if sub.pushRegID == "" {
			// No push reg ID — clean up after staleNoPushTimeout
			if time.Since(sub.bufferStart) > staleNoPushTimeout {
				delete(m.subscriptions, key)
				slog.Info("ws: cleaned up stale subscription (no push)", "client_id", key, "disconnected_for", time.Since(sub.bufferStart))
			}
		} else {
			// Has push reg ID — clean up if no WS connection within staleWithPushTimeout
			// lastActive is updated on every Subscribe, so it tracks the most recent connection
			if time.Since(sub.lastActive) > staleWithPushTimeout {
				delete(m.subscriptions, key)
				slog.Info("ws: cleaned up stale subscription (with push, no connect in 10 days)", "client_id", key, "last_active", sub.lastActive)
			}
		}
		sub.mu.Unlock()
	}
}

// eventSeq is an atomic counter to ensure unique event IDs.
var eventSeq atomic.Int64

// truncateForPush truncates s to pushAlertMaxRunes, appending "…" if truncated.
func truncateForPush(s string) string {
	if utf8.RuneCountInString(s) <= pushAlertMaxRunes {
		return s
	}
	return string([]rune(s)[:pushAlertMaxRunes]) + "…"
}

// GenerateEventID creates a unique event ID.
// Uses an atomic counter instead of exposing server timestamps.
func GenerateEventID() string {
	return fmt.Sprintf("evt_%d", eventSeq.Add(1))
}
