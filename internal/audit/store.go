// Package audit implements an embedded high-performance SQLite (WAL + FTS5)
// activity and audit store with content-addressed image blobs and live SSE fan-out.
package audit

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxBlobBytes   = 8 << 20
	maxSubscribers = 64
)

var validBlobHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Session summarizes one logical agent/client timeline.
type Session struct {
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	ClientName string `json:"client_name,omitempty"`
	Wire       string `json:"wire"`
	Title      string `json:"title,omitempty"`
	Status     string `json:"status"` // "active", "working", "waiting", "done", "idle", "error", "closed"
	ToolCount  int    `json:"tool_count"`
	ErrorCount int    `json:"error_count"`
	StartedAt  int64  `json:"started_at"`
	LastSeenAt int64  `json:"last_seen_at"`
	EndedAt    int64  `json:"ended_at,omitempty"`
}

// Event is one immutable-sequence entry on a session's timeline.
type Event struct {
	ID         int64  `json:"id"`
	SessionID  string `json:"session_id"`
	AgentID    string `json:"agent_id"`
	Wire       string `json:"wire"`
	Kind       string `json:"kind"` // "tool_call", "note", "proposal", "system"
	ToolName   string `json:"tool_name,omitempty"`
	Summary    string `json:"summary"`
	ArgsJSON   string `json:"args_json,omitempty"`
	ResultJSON string `json:"result_json,omitempty"`
	ImageBlob  string `json:"image_blob,omitempty"`
	ImageMIME  string `json:"image_mime,omitempty"`
	Status     string `json:"status"` // "running", "ok", "error", "canceled", "info"
	ErrorText  string `json:"error_text,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

// LivePacket is fanned out to SSE subscribers in real time.
type LivePacket struct {
	Type    string   `json:"type"` // "event", "session"
	Event   *Event   `json:"event,omitempty"`
	Session *Session `json:"session,omitempty"`
}

// EventFilter constrains ListEvents queries.
type EventFilter struct {
	SessionID string
	AgentID   string
	Wire      string
	Kind      string
	Status    string
	BeforeID  int64
	AfterID   int64
	Limit     int
	Compact   bool
}

// Stats reports store-level observability metrics.
type Stats struct {
	Sessions       int   `json:"sessions"`
	ActiveSessions int   `json:"active_sessions"`
	Events         int   `json:"events"`
	Errors         int   `json:"errors"`
	DBBytes        int64 `json:"db_bytes"`
}

// StartToolInput describes the beginning of one audited tool call.
type StartToolInput struct {
	SessionID  string
	AgentID    string
	ClientName string
	Wire       string
	ToolName   string
	RawArgs    json.RawMessage
	Kind       string // defaults to "tool_call"
}

// FinishToolInput describes the completion of an audited tool call.
type FinishToolInput struct {
	Status     string // "ok", "error", "canceled"
	Result     any
	ImageData  []byte
	ImageMIME  string
	ErrorText  string
	DurationMS int64
}

// NoteInput describes a lightweight progress update from agent_report.
type NoteInput struct {
	SessionID  string
	AgentID    string
	ClientName string
	Wire       string
	Title      string
	Message    string
	Status     string
}

// Store owns the embedded SQLite WAL database, optional blob directory, and
// real-time subscriber hub.
type Store struct {
	db      *sql.DB
	dbPath  string
	blobDir string

	blobMu   sync.RWMutex
	memBlobs map[string][]byte

	subMu       sync.Mutex
	subscribers map[chan LivePacket]struct{}
	closed      bool
}

// Open initializes (or opens) the SQLite WAL + FTS5 audit store.
// Passing dbPath == "" opens an isolated in-memory database suitable for tests.
func Open(dbPath string) (*Store, error) {
	dsn := "file::memory:?cache=shared"
	blobDir := ""
	if dbPath != "" {
		abs, err := filepath.Abs(dbPath)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return nil, err
		}
		dbPath = abs
		blobDir = filepath.Join(filepath.Dir(abs), "lrmcp-blobs")
		dsn = abs
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open audit db: %w", err)
	}
	// Single writer pool serializes SQLite writes cleanly with zero SQLITE_BUSY
	// storms under high multi-agent concurrency while WAL allows instant reads.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA cache_size = -16000;",
	}
	if dbPath != "" {
		pragmas = append([]string{"PRAGMA journal_mode = WAL;", "PRAGMA synchronous = NORMAL;"}, pragmas...)
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("audit pragma %q: %w", p, err)
		}
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{
		db:          db,
		dbPath:      dbPath,
		blobDir:     blobDir,
		memBlobs:    map[string][]byte{},
		subscribers: map[chan LivePacket]struct{}{},
	}, nil
}

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	agent_id TEXT NOT NULL,
	client_name TEXT NOT NULL DEFAULT '',
	wire TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'active',
	tool_count INTEGER NOT NULL DEFAULT 0,
	error_count INTEGER NOT NULL DEFAULT 0,
	started_at INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	ended_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sessions_last_seen ON sessions(last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_agent ON sessions(agent_id);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	agent_id TEXT NOT NULL,
	wire TEXT NOT NULL,
	kind TEXT NOT NULL,
	tool_name TEXT NOT NULL DEFAULT '',
	summary TEXT NOT NULL DEFAULT '',
	args_json TEXT NOT NULL DEFAULT '{}',
	result_json TEXT NOT NULL DEFAULT '{}',
	image_blob TEXT NOT NULL DEFAULT '',
	image_mime TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'ok',
	error_text TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_session_id ON events(session_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events(kind, id DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(
	summary,
	tool_name,
	args_json,
	result_json,
	error_text,
	agent_id,
	content='events',
	content_rowid='id',
	tokenize='trigram'
);

CREATE TRIGGER IF NOT EXISTS events_ai AFTER INSERT ON events BEGIN
	INSERT INTO events_fts(rowid, summary, tool_name, args_json, result_json, error_text, agent_id)
	VALUES (new.id, new.summary, new.tool_name, new.args_json, new.result_json, new.error_text, new.agent_id);
END;

CREATE TRIGGER IF NOT EXISTS events_ad AFTER DELETE ON events BEGIN
	INSERT INTO events_fts(events_fts, rowid, summary, tool_name, args_json, result_json, error_text, agent_id)
	VALUES ('delete', old.id, old.summary, old.tool_name, old.args_json, old.result_json, old.error_text, old.agent_id);
END;

CREATE TRIGGER IF NOT EXISTS events_au AFTER UPDATE ON events BEGIN
	INSERT INTO events_fts(events_fts, rowid, summary, tool_name, args_json, result_json, error_text, agent_id)
	VALUES ('delete', old.id, old.summary, old.tool_name, old.args_json, old.result_json, old.error_text, old.agent_id);
	INSERT INTO events_fts(rowid, summary, tool_name, args_json, result_json, error_text, agent_id)
	VALUES (new.id, new.summary, new.tool_name, new.args_json, new.result_json, new.error_text, new.agent_id);
END;
`
	_, err := db.Exec(schema)
	return err
}

func normalizeIDs(sessionID, agentID, clientName, wire string) (string, string, string) {
	wire = strings.TrimSpace(wire)
	if wire == "" {
		wire = "local"
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		agentID = strings.TrimSpace(clientName)
	}
	if agentID == "" {
		agentID = wire
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = wire + ":" + agentID
	}
	return sessionID, agentID, wire
}

func (s *Store) upsertSession(id, agentID, clientName, wire, title, status string, now int64, incTool, incErr int) (Session, error) {
	if status == "" {
		status = "active"
	}
	_, err := s.db.Exec(`
		INSERT INTO sessions (id, agent_id, client_name, wire, title, status, tool_count, error_count, started_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			agent_id = CASE WHEN excluded.agent_id != '' THEN excluded.agent_id ELSE sessions.agent_id END,
			client_name = CASE WHEN excluded.client_name != '' THEN excluded.client_name ELSE sessions.client_name END,
			title = CASE WHEN excluded.title != '' THEN excluded.title ELSE sessions.title END,
			status = excluded.status,
			tool_count = sessions.tool_count + excluded.tool_count,
			error_count = sessions.error_count + excluded.error_count,
			last_seen_at = excluded.last_seen_at
	`, id, agentID, clientName, wire, title, status, incTool, incErr, now, now)
	if err != nil {
		return Session{}, err
	}
	return s.getSession(id)
}

func (s *Store) getSession(id string) (Session, error) {
	var out Session
	err := s.db.QueryRow(`
		SELECT id, agent_id, client_name, wire, title, status, tool_count, error_count, started_at, last_seen_at, ended_at
		FROM sessions WHERE id = ?
	`, id).Scan(&out.ID, &out.AgentID, &out.ClientName, &out.Wire, &out.Title, &out.Status, &out.ToolCount, &out.ErrorCount, &out.StartedAt, &out.LastSeenAt, &out.EndedAt)
	return out, err
}

// StartTool records the start of a tool invocation and broadcasts it live.
func (s *Store) StartTool(in StartToolInput) (int64, string) {
	if s == nil {
		return 0, ""
	}
	now := time.Now().UnixMilli()
	sid, aid, wire := normalizeIDs(in.SessionID, in.AgentID, in.ClientName, in.Wire)
	kind := in.Kind
	if kind == "" {
		if in.ToolName == "capability_propose" {
			kind = "proposal"
		} else {
			kind = "tool_call"
		}
	}
	args := RedactRaw(in.RawArgs)
	summary := SummarizeTool(in.ToolName, in.RawArgs)

	sess, err := s.upsertSession(sid, aid, in.ClientName, wire, "", "working", now, 1, 0)
	if err != nil {
		return 0, sid
	}
	res, err := s.db.Exec(`
		INSERT INTO events (session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '{}', 'running', ?)
	`, sid, aid, wire, kind, in.ToolName, summary, args, now)
	if err != nil {
		return 0, sid
	}
	id, _ := res.LastInsertId()
	ev := Event{
		ID:        id,
		SessionID: sid,
		AgentID:   aid,
		Wire:      wire,
		Kind:      kind,
		ToolName:  in.ToolName,
		Summary:   summary,
		ArgsJSON:  args,
		Status:    "running",
		CreatedAt: now,
	}
	s.broadcast(LivePacket{Type: "event", Event: &ev, Session: &sess})
	return id, sid
}

// FinishTool updates an in-flight tool event with its redacted result, optional
// content-addressed image blob, duration, and final status.
func (s *Store) FinishTool(eventID int64, sessionID string, in FinishToolInput) {
	if s == nil || eventID <= 0 {
		return
	}
	now := time.Now().UnixMilli()
	status := in.Status
	if status == "" {
		status = "ok"
	}
	resultJSON := RedactAny(in.Result)
	errText := in.ErrorText
	if len(errText) > 4096 {
		errText = errText[:4096] + "…"
	}
	blobHash := ""
	blobMIME := ""
	if len(in.ImageData) > 0 && len(in.ImageData) <= maxBlobBytes && strings.HasPrefix(in.ImageMIME, "image/") {
		if h, err := s.saveBlob(in.ImageData); err == nil {
			blobHash = h
			blobMIME = in.ImageMIME
		}
	}
	_, err := s.db.Exec(`
		UPDATE events
		SET status = ?, result_json = ?, image_blob = ?, image_mime = ?, error_text = ?, duration_ms = ?
		WHERE id = ?
	`, status, resultJSON, blobHash, blobMIME, errText, in.DurationMS, eventID)
	if err != nil {
		return
	}
	incErr := 0
	sessStatus := "active"
	if status == "error" {
		incErr = 1
		sessStatus = "error"
	}
	if sessionID != "" {
		_, _ = s.db.Exec(`
			UPDATE sessions
			SET status = ?, error_count = error_count + ?, last_seen_at = ?
			WHERE id = ?
		`, sessStatus, incErr, now, sessionID)
	}
	ev, err := s.getEvent(eventID)
	if err != nil {
		return
	}
	var sessPtr *Session
	if sess, err := s.getSession(ev.SessionID); err == nil {
		sessPtr = &sess
	}
	s.broadcast(LivePacket{Type: "event", Event: &ev, Session: sessPtr})
}

// RecordNote persists a human-readable progress milestone from agent_report.
func (s *Store) RecordNote(in NoteInput) (Event, error) {
	if s == nil {
		return Event{}, nil
	}
	now := time.Now().UnixMilli()
	sid, aid, wire := normalizeIDs(in.SessionID, in.AgentID, in.ClientName, in.Wire)
	msg := strings.TrimSpace(in.Message)
	if len(msg) > 4000 {
		msg = msg[:4000] + "…"
	}
	title := strings.TrimSpace(in.Title)
	if len(title) > 160 {
		title = title[:160]
	}
	st := strings.TrimSpace(in.Status)
	if st == "" {
		st = "working"
	}
	sess, err := s.upsertSession(sid, aid, in.ClientName, wire, title, st, now, 0, 0)
	if err != nil {
		return Event{}, err
	}
	argsJSON := RedactAny(map[string]any{"title": title, "status": st, "message": msg})
	res, err := s.db.Exec(`
		INSERT INTO events (session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, status, created_at)
		VALUES (?, ?, ?, 'note', 'agent_report', ?, ?, '{}', ?, ?)
	`, sid, aid, wire, msg, argsJSON, st, now)
	if err != nil {
		return Event{}, err
	}
	id, _ := res.LastInsertId()
	ev := Event{
		ID:        id,
		SessionID: sid,
		AgentID:   aid,
		Wire:      wire,
		Kind:      "note",
		ToolName:  "agent_report",
		Summary:   msg,
		ArgsJSON:  argsJSON,
		Status:    st,
		CreatedAt: now,
	}
	s.broadcast(LivePacket{Type: "event", Event: &ev, Session: &sess})
	return ev, nil
}

// RecordSystem logs a runtime lifecycle event (tunnel attach/detach, desktop kill, proposal decision).
func (s *Store) RecordSystem(kind, summary string, detail any) {
	if s == nil {
		return
	}
	if kind == "" {
		kind = "system"
	}
	now := time.Now().UnixMilli()
	sid := "system:daemon"
	sess, err := s.upsertSession(sid, "lrmcp-daemon", "lrmcp", "system", "System Events", "active", now, 0, 0)
	if err != nil {
		return
	}
	argsJSON := RedactAny(detail)
	res, err := s.db.Exec(`
		INSERT INTO events (session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, status, created_at)
		VALUES (?, 'lrmcp-daemon', 'system', ?, '', ?, ?, '{}', 'info', ?)
	`, sid, kind, summary, argsJSON, now)
	if err != nil {
		return
	}
	id, _ := res.LastInsertId()
	ev := Event{
		ID:        id,
		SessionID: sid,
		AgentID:   "lrmcp-daemon",
		Wire:      "system",
		Kind:      kind,
		Summary:   summary,
		ArgsJSON:  argsJSON,
		Status:    "info",
		CreatedAt: now,
	}
	s.broadcast(LivePacket{Type: "event", Event: &ev, Session: &sess})
}

// GetEvent retrieves one complete audit event by its numeric ID.
func (s *Store) GetEvent(id int64) (Event, error) {
	if s == nil {
		return Event{}, sql.ErrNoRows
	}
	return s.getEvent(id)
}

// BlobPath returns the on-disk file path for a stored blob hash, if persisted.
func (s *Store) BlobPath(hash string) string {
	if s == nil || s.blobDir == "" || !validBlobHash.MatchString(hash) {
		return ""
	}
	target := filepath.Join(s.blobDir, hash)
	if _, err := os.Stat(target); err == nil {
		return target
	}
	return ""
}

func (s *Store) getEvent(id int64) (Event, error) {
	var e Event
	err := s.db.QueryRow(`
		SELECT id, session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, image_blob, image_mime, status, error_text, duration_ms, created_at
		FROM events WHERE id = ?
	`, id).Scan(&e.ID, &e.SessionID, &e.AgentID, &e.Wire, &e.Kind, &e.ToolName, &e.Summary, &e.ArgsJSON, &e.ResultJSON, &e.ImageBlob, &e.ImageMIME, &e.Status, &e.ErrorText, &e.DurationMS, &e.CreatedAt)
	return e, err
}

// ListSessions returns the most recently active sessions.
func (s *Store) ListSessions(limit int) ([]Session, error) {
	if s == nil {
		return []Session{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`
		SELECT id, agent_id, client_name, wire, title, status, tool_count, error_count, started_at, last_seen_at, ended_at
		FROM sessions ORDER BY last_seen_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Session, 0)
	for rows.Next() {
		var item Session
		if err := rows.Scan(&item.ID, &item.AgentID, &item.ClientName, &item.Wire, &item.Title, &item.Status, &item.ToolCount, &item.ErrorCount, &item.StartedAt, &item.LastSeenAt, &item.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListEvents returns filtered timeline events ordered chronologically (oldest first for a session, or newest-N in chronological order).
func (s *Store) ListEvents(f EventFilter) ([]Event, error) {
	if s == nil {
		return []Event{}, nil
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var where []string
	var args []any
	if f.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.AgentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, f.AgentID)
	}
	if f.Wire != "" {
		where = append(where, "wire = ?")
		args = append(args, f.Wire)
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, f.BeforeID)
	}
	if f.AfterID > 0 {
		where = append(where, "id > ?")
		args = append(args, f.AfterID)
	}
	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}
	cols := "id, session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, image_blob, image_mime, status, error_text, duration_ms, created_at"
	if f.Compact {
		cols = "id, session_id, agent_id, wire, kind, tool_name, summary, '', '', image_blob, image_mime, status, error_text, duration_ms, created_at"
	}
	query := fmt.Sprintf(`
		SELECT %s
		FROM events %s ORDER BY id DESC LIMIT ?
	`, cols, clause)
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Event, 0)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.SessionID, &e.AgentID, &e.Wire, &e.Kind, &e.ToolName, &e.Summary, &e.ArgsJSON, &e.ResultJSON, &e.ImageBlob, &e.ImageMIME, &e.Status, &e.ErrorText, &e.DurationMS, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to chronological ascending order for timeline rendering.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Search executes a millisecond SQLite FTS5 trigram full-text search across all recorded events
// (falling back cleanly to LIKE for 1-2 character queries).
func (s *Store) Search(query string, limit int) ([]Event, error) {
	return s.SearchWithOptions(query, limit, false)
}

// SearchWithOptions executes full-text search and optionally omits bulky args/result payloads.
func (s *Store) SearchWithOptions(query string, limit int, compact bool) ([]Event, error) {
	if s == nil {
		return []Event{}, nil
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return s.ListEvents(EventFilter{Limit: limit, Compact: compact})
	}
	if limit <= 0 || limit > 200 {
		limit = 80
	}
	var (
		rows *sql.Rows
		err  error
	)
	ftsCols := "e.id, e.session_id, e.agent_id, e.wire, e.kind, e.tool_name, e.summary, e.args_json, e.result_json, e.image_blob, e.image_mime, e.status, e.error_text, e.duration_ms, e.created_at"
	plainCols := "id, session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, image_blob, image_mime, status, error_text, duration_ms, created_at"
	if compact {
		ftsCols = "e.id, e.session_id, e.agent_id, e.wire, e.kind, e.tool_name, e.summary, '', '', e.image_blob, e.image_mime, e.status, e.error_text, e.duration_ms, e.created_at"
		plainCols = "id, session_id, agent_id, wire, kind, tool_name, summary, '', '', image_blob, image_mime, status, error_text, duration_ms, created_at"
	}
	useFTS := len([]rune(q)) >= 3
	if useFTS {
		// Quote user input safely for FTS5 trigram phrase matching.
		safeFTS := `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
		rows, err = s.db.Query(fmt.Sprintf(`
			SELECT %s
			FROM events_fts f
			JOIN events e ON e.id = f.rowid
			WHERE events_fts MATCH ?
			ORDER BY e.id DESC
			LIMIT ?
		`, ftsCols), safeFTS, limit)
	}
	if !useFTS || err != nil {
		like := "%" + q + "%"
		rows, err = s.db.Query(fmt.Sprintf(`
			SELECT %s
			FROM events
			WHERE summary LIKE ? OR tool_name LIKE ? OR args_json LIKE ? OR result_json LIKE ? OR error_text LIKE ? OR agent_id LIKE ?
			ORDER BY id DESC
			LIMIT ?
		`, plainCols), like, like, like, like, like, like, limit)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()
	out := make([]Event, 0)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.SessionID, &e.AgentID, &e.Wire, &e.Kind, &e.ToolName, &e.Summary, &e.ArgsJSON, &e.ResultJSON, &e.ImageBlob, &e.ImageMIME, &e.Status, &e.ErrorText, &e.DurationMS, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkSessionClosed marks a session as closed and notifies SSE listeners.
func (s *Store) MarkSessionClosed(sessionID string) {
	if s == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	now := time.Now().UnixMilli()
	_, _ = s.db.Exec(`UPDATE sessions SET status = 'closed', ended_at = ?, last_seen_at = ? WHERE id = ?`, now, now, sessionID)
	if sess, err := s.getSession(sessionID); err == nil {
		s.broadcast(LivePacket{Type: "session", Session: &sess})
	}
}

// Stats returns aggregate audit database statistics.
func (s *Store) Stats() (Stats, error) {
	if s == nil {
		return Stats{}, nil
	}
	var st Stats
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&st.Sessions)
	cutoff := time.Now().Add(-5 * time.Minute).UnixMilli()
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE last_seen_at >= ?`, cutoff).Scan(&st.ActiveSessions)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&st.Events)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE status = 'error'`).Scan(&st.Errors)
	if s.dbPath != "" {
		if info, err := os.Stat(s.dbPath); err == nil {
			st.DBBytes = info.Size()
		}
	}
	return st, nil
}

// ExportJSONL writes matching events as standard newline-delimited JSON.
func (s *Store) ExportJSONL(w io.Writer, sessionID string) error {
	if s == nil {
		return nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	if sessionID != "" {
		rows, err = s.db.Query(`
			SELECT id, session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, image_blob, image_mime, status, error_text, duration_ms, created_at
			FROM events WHERE session_id = ? ORDER BY id ASC
		`, sessionID)
	} else {
		rows, err = s.db.Query(`
			SELECT id, session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, image_blob, image_mime, status, error_text, duration_ms, created_at
			FROM events ORDER BY id ASC
		`)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.SessionID, &e.AgentID, &e.Wire, &e.Kind, &e.ToolName, &e.Summary, &e.ArgsJSON, &e.ResultJSON, &e.ImageBlob, &e.ImageMIME, &e.Status, &e.ErrorText, &e.DurationMS, &e.CreatedAt); err != nil {
			return err
		}
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) saveBlob(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if s.blobDir == "" {
		s.blobMu.Lock()
		if len(s.memBlobs) < 64 {
			s.memBlobs[hash] = append([]byte(nil), data...)
		}
		s.blobMu.Unlock()
		return hash, nil
	}
	if err := os.MkdirAll(s.blobDir, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(s.blobDir, hash)
	if _, err := os.Stat(target); err == nil {
		return hash, nil
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return "", err
	}
	return hash, nil
}

// GetBlob retrieves a content-addressed image blob by its SHA-256 hex digest.
func (s *Store) GetBlob(hash string) ([]byte, error) {
	if s == nil || !validBlobHash.MatchString(hash) {
		return nil, errors.New("invalid blob hash")
	}
	if s.blobDir == "" {
		s.blobMu.RLock()
		data, ok := s.memBlobs[hash]
		s.blobMu.RUnlock()
		if !ok {
			return nil, os.ErrNotExist
		}
		return data, nil
	}
	return os.ReadFile(filepath.Join(s.blobDir, hash))
}

// Subscribe opens a real-time buffered live stream for SSE clients.
func (s *Store) Subscribe() (<-chan LivePacket, func()) {
	ch := make(chan LivePacket, 128)
	s.subMu.Lock()
	if s.closed || len(s.subscribers) >= maxSubscribers {
		s.subMu.Unlock()
		close(ch)
		return ch, func() {}
	}
	s.subscribers[ch] = struct{}{}
	s.subMu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.subMu.Lock()
			if _, ok := s.subscribers[ch]; ok {
				delete(s.subscribers, ch)
				close(ch)
			}
			s.subMu.Unlock()
		})
	}
}

func (s *Store) broadcast(pkt LivePacket) {
	s.subMu.Lock()
	for ch := range s.subscribers {
		select {
		case ch <- pkt:
		default:
			// Drop for slow consumers; never block MCP execution.
		}
	}
	s.subMu.Unlock()
}

// Close closes subscribers and flushes SQLite cleanly.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.subMu.Lock()
	if !s.closed {
		s.closed = true
		for ch := range s.subscribers {
			close(ch)
			delete(s.subscribers, ch)
		}
	}
	s.subMu.Unlock()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
