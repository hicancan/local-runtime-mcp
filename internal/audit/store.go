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
	maxBlobBytes      = 8 << 20
	maxSubscribers    = 64
	sessionIdleAfter  = 15 * time.Minute
	ftsExcerptBytes   = 1024
	maxRetainedEvents = 20_000
	maxEventAge       = 90 * 24 * time.Hour
	orphanBlobGrace   = 5 * time.Minute
	auditSchemaVer    = 3
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
	Type    string   `json:"type"` // "event", "session", "refresh"
	Event   *Event   `json:"event,omitempty"`
	Session *Session `json:"session,omitempty"`
}

// DeleteRequest selects agent timelines and/or individual sessions to purge.
type DeleteRequest struct {
	AgentIDs   []string `json:"agent_ids,omitempty"`
	SessionIDs []string `json:"session_ids,omitempty"`
	Vacuum     bool     `json:"vacuum,omitempty"`
}

// DeleteResult summarizes sessions, events, and unreferenced blobs purged from the store.
type DeleteResult struct {
	Sessions int64 `json:"sessions"`
	Events   int64 `json:"events"`
	Blobs    int   `json:"blobs"`
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
	Status       string // "ok", "error", "canceled"
	FinalSummary string // optional completed summary (e.g. run.done_title)
	Result       any
	ImageData    []byte
	ImageMIME    string
	ErrorText    string
	DurationMS   int64
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

type inFlightMeta struct {
	sessionID string
	agentID   string
	wire      string
	kind      string
	toolName  string
	summary   string
	argsJSON  string
	createdAt int64
}

// Store owns the embedded SQLite WAL database, optional blob directory, and
// real-time subscriber hub.
type Store struct {
	db      *sql.DB
	dbPath  string
	blobDir string

	inFlightMu sync.Mutex
	inFlight   map[int64]inFlightMeta

	blobMu   sync.RWMutex
	memBlobs map[string][]byte

	subMu       sync.Mutex
	subscribers map[chan LivePacket]struct{}
	closed      bool

	stopPrune chan struct{}
	pruneDone chan struct{}
}

// Open initializes (or opens) the SQLite WAL + FTS5 audit store.
// Passing dbPath == "" opens an isolated in-memory database suitable for tests.
func Open(dbPath string) (*Store, error) {
	dsn := ":memory:"
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
		"PRAGMA temp_store = MEMORY;",
	}
	if dbPath != "" {
		pragmas = append([]string{
			"PRAGMA auto_vacuum = INCREMENTAL;",
			"PRAGMA journal_mode = WAL;",
			"PRAGMA synchronous = NORMAL;",
			"PRAGMA journal_size_limit = 8388608;",
		}, pragmas...)
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("audit pragma %q: %w", p, err)
		}
	}
	if err := migrate(db, dbPath != ""); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &Store{
		db:          db,
		dbPath:      dbPath,
		blobDir:     blobDir,
		inFlight:    map[int64]inFlightMeta{},
		memBlobs:    map[string][]byte{},
		subscribers: map[chan LivePacket]struct{}{},
	}
	if dbPath != "" {
		store.stopPrune = make(chan struct{})
		store.pruneDone = make(chan struct{})
		go store.pruneLoop()
	}
	return store, nil
}

func ftsExcerpt(s string) string {
	if len(s) <= ftsExcerptBytes {
		return s
	}
	return truncateUTF8(s, ftsExcerptBytes)
}

func migrate(db *sql.DB, onDisk bool) error {
	const baseSchema = `
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
CREATE INDEX IF NOT EXISTS idx_events_status ON events(status);
CREATE INDEX IF NOT EXISTS idx_events_agent ON events(agent_id);
`
	if _, err := db.Exec(baseSchema); err != nil {
		return err
	}

	var userVer int
	_ = db.QueryRow(`PRAGMA user_version;`).Scan(&userVer)
	if userVer >= auditSchemaVer {
		return nil
	}

	// Migrate legacy trigger-driven external-content FTS5 (which caused 3x write
	// amplification per tool call and indexed full 48 KB JSON blobs) to a compact
	// standalone FTS5 table populated once per completed event with bounded excerpts.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const ftsMigration = `
DROP TRIGGER IF EXISTS events_ai;
DROP TRIGGER IF EXISTS events_ad;
DROP TRIGGER IF EXISTS events_au;
DROP TABLE IF EXISTS events_fts;
CREATE VIRTUAL TABLE events_fts USING fts5(
	summary,
	tool_name,
	args_json,
	result_json,
	error_text,
	agent_id,
	content='',
	contentless_delete=1,
	tokenize='trigram'
);
`
	if _, err := tx.Exec(ftsMigration); err != nil {
		return err
	}

	// Backfill existing events into the compact standalone FTS5 index.
	rows, err := tx.Query(`SELECT id, summary, tool_name, args_json, result_json, error_text, agent_id FROM events`)
	if err != nil {
		return err
	}
	type ftsRow struct {
		id         int64
		summary    string
		toolName   string
		argsJSON   string
		resultJSON string
		errorText  string
		agentID    string
	}
	var existing []ftsRow
	for rows.Next() {
		var r ftsRow
		if err := rows.Scan(&r.id, &r.summary, &r.toolName, &r.argsJSON, &r.resultJSON, &r.errorText, &r.agentID); err != nil {
			_ = rows.Close()
			return err
		}
		existing = append(existing, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(existing) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO events_fts(rowid, summary, tool_name, args_json, result_json, error_text, agent_id) VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		for _, r := range existing {
			if _, err := stmt.Exec(r.id, r.summary, r.toolName, ftsExcerpt(r.argsJSON), ftsExcerpt(r.resultJSON), r.errorText, r.agentID); err != nil {
				_ = stmt.Close()
				return err
			}
		}
		_ = stmt.Close()
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d;", auditSchemaVer)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// Reclaim legacy FTS5 bloat and activate incremental auto_vacuum on existing disk DBs.
	if onDisk && len(existing) > 0 {
		var av int
		_ = db.QueryRow(`PRAGMA auto_vacuum;`).Scan(&av)
		if av != 2 {
			_, _ = db.Exec(`PRAGMA auto_vacuum = INCREMENTAL;`)
		}
		_, _ = db.Exec(`VACUUM;`)
		_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE);`)
	}
	return nil
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

func (s *Store) hasSubscribers() bool {
	s.subMu.Lock()
	n := len(s.subscribers)
	s.subMu.Unlock()
	return n > 0
}

type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func upsertSessionExec(ex sqlExecer, id, agentID, clientName, wire, title, status string, now int64, incTool, incErr int) error {
	if status == "" {
		status = "active"
	}
	_, err := ex.Exec(`
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
	return err
}

func insertFTSEntry(ex sqlExecer, id int64, summary, toolName, argsJSON, resultJSON, errorText, agentID string) {
	_, _ = ex.Exec(`DELETE FROM events_fts WHERE rowid = ?`, id)
	_, _ = ex.Exec(`
		INSERT INTO events_fts(rowid, summary, tool_name, args_json, result_json, error_text, agent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, summary, toolName, ftsExcerpt(argsJSON), ftsExcerpt(resultJSON), errorText, agentID)
}

func normalizeSessionStatus(s Session, nowMS int64) Session {
	if (s.Status == "active" || s.Status == "working") && s.Wire != "system" && s.LastSeenAt > 0 && nowMS-s.LastSeenAt > sessionIdleAfter.Milliseconds() {
		s.Status = "idle"
	}
	return s
}

func (s *Store) getSession(id string) (Session, error) {
	var out Session
	err := s.db.QueryRow(`
		SELECT id, agent_id, client_name, wire, title, status, tool_count, error_count, started_at, last_seen_at, ended_at
		FROM sessions WHERE id = ?
	`, id).Scan(&out.ID, &out.AgentID, &out.ClientName, &out.Wire, &out.Title, &out.Status, &out.ToolCount, &out.ErrorCount, &out.StartedAt, &out.LastSeenAt, &out.EndedAt)
	if err != nil {
		return out, err
	}
	return normalizeSessionStatus(out, time.Now().UnixMilli()), nil
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

	tx, err := s.db.Begin()
	if err != nil {
		return 0, sid
	}
	if err := upsertSessionExec(tx, sid, aid, in.ClientName, wire, "", "working", now, 1, 0); err != nil {
		_ = tx.Rollback()
		return 0, sid
	}
	res, err := tx.Exec(`
		INSERT INTO events (session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '{}', 'running', ?)
	`, sid, aid, wire, kind, in.ToolName, summary, args, now)
	if err != nil {
		_ = tx.Rollback()
		return 0, sid
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return 0, sid
	}

	s.inFlightMu.Lock()
	if len(s.inFlight) >= 4096 {
		clear(s.inFlight)
	}
	s.inFlight[id] = inFlightMeta{
		sessionID: sid,
		agentID:   aid,
		wire:      wire,
		kind:      kind,
		toolName:  in.ToolName,
		summary:   summary,
		argsJSON:  args,
		createdAt: now,
	}
	s.inFlightMu.Unlock()

	if s.hasSubscribers() {
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
		var sessPtr *Session
		if sess, err := s.getSession(sid); err == nil {
			sessPtr = &sess
		}
		s.broadcast(LivePacket{Type: "event", Event: &ev, Session: sessPtr})
	}
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
		errText = truncateUTF8(errText, 4096) + "…"
	}
	blobHash := ""
	blobMIME := ""
	if len(in.ImageData) > 0 && len(in.ImageData) <= maxBlobBytes && strings.HasPrefix(in.ImageMIME, "image/") {
		if h, err := s.saveBlob(in.ImageData); err == nil {
			blobHash = h
			blobMIME = in.ImageMIME
		}
	}
	finalSummary := strings.TrimSpace(in.FinalSummary)

	s.inFlightMu.Lock()
	meta, hasMeta := s.inFlight[eventID]
	if hasMeta {
		delete(s.inFlight, eventID)
	}
	s.inFlightMu.Unlock()

	incErr := 0
	sessStatus := "active"
	if status == "error" {
		incErr = 1
		sessStatus = "error"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	var res sql.Result
	if finalSummary != "" {
		res, err = tx.Exec(`
			UPDATE events
			SET status = ?, summary = ?, result_json = ?, image_blob = ?, image_mime = ?, error_text = ?, duration_ms = ?
			WHERE id = ?
		`, status, finalSummary, resultJSON, blobHash, blobMIME, errText, in.DurationMS, eventID)
	} else {
		res, err = tx.Exec(`
			UPDATE events
			SET status = ?, result_json = ?, image_blob = ?, image_mime = ?, error_text = ?, duration_ms = ?
			WHERE id = ?
		`, status, resultJSON, blobHash, blobMIME, errText, in.DurationMS, eventID)
	}
	if err != nil {
		_ = tx.Rollback()
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		_ = tx.Rollback()
		return
	}
	if sessionID != "" {
		_, _ = tx.Exec(`
			UPDATE sessions
			SET status = CASE WHEN status IN ('done', 'closed') THEN status ELSE ? END,
				error_count = error_count + ?,
				last_seen_at = ?
			WHERE id = ?
		`, sessStatus, incErr, now, sessionID)
	}
	if hasMeta {
		summary := meta.summary
		if finalSummary != "" {
			summary = finalSummary
		}
		insertFTSEntry(tx, eventID, summary, meta.toolName, meta.argsJSON, resultJSON, errText, meta.agentID)
	}
	if err := tx.Commit(); err != nil {
		return
	}
	if !hasMeta {
		if ev, err := s.getEvent(eventID); err == nil {
			insertFTSEntry(s.db, ev.ID, ev.Summary, ev.ToolName, ev.ArgsJSON, ev.ResultJSON, ev.ErrorText, ev.AgentID)
			if s.hasSubscribers() {
				var sessPtr *Session
				if sess, err := s.getSession(ev.SessionID); err == nil {
					sessPtr = &sess
				}
				s.broadcast(LivePacket{Type: "event", Event: &ev, Session: sessPtr})
			}
		}
		return
	}
	if !s.hasSubscribers() {
		return
	}
	summary := meta.summary
	if finalSummary != "" {
		summary = finalSummary
	}
	ev := Event{
		ID:         eventID,
		SessionID:  meta.sessionID,
		AgentID:    meta.agentID,
		Wire:       meta.wire,
		Kind:       meta.kind,
		ToolName:   meta.toolName,
		Summary:    summary,
		ArgsJSON:   meta.argsJSON,
		ResultJSON: resultJSON,
		ImageBlob:  blobHash,
		ImageMIME:  blobMIME,
		Status:     status,
		ErrorText:  errText,
		DurationMS: in.DurationMS,
		CreatedAt:  meta.createdAt,
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
		msg = truncateUTF8(msg, 4000) + "…"
	}
	title := strings.TrimSpace(in.Title)
	if len(title) > 160 {
		title = truncateUTF8(title, 160)
	}
	st := strings.TrimSpace(in.Status)
	if st == "" {
		st = "working"
	}
	argsJSON := RedactAny(map[string]any{"title": title, "status": st, "message": msg})

	tx, err := s.db.Begin()
	if err != nil {
		return Event{}, err
	}
	if err := upsertSessionExec(tx, sid, aid, in.ClientName, wire, title, st, now, 0, 0); err != nil {
		_ = tx.Rollback()
		return Event{}, err
	}
	res, err := tx.Exec(`
		INSERT INTO events (session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, status, created_at)
		VALUES (?, ?, ?, 'note', 'session', ?, ?, '{}', ?, ?)
	`, sid, aid, wire, msg, argsJSON, st, now)
	if err != nil {
		_ = tx.Rollback()
		return Event{}, err
	}
	if st == "done" || st == "closed" {
		_, _ = tx.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ?`, now, sid)
	}
	id, _ := res.LastInsertId()
	insertFTSEntry(tx, id, msg, "session", argsJSON, "{}", "", aid)
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}

	ev := Event{
		ID:        id,
		SessionID: sid,
		AgentID:   aid,
		Wire:      wire,
		Kind:      "note",
		ToolName:  "session",
		Summary:   msg,
		ArgsJSON:  argsJSON,
		Status:    st,
		CreatedAt: now,
	}
	if s.hasSubscribers() {
		var sessPtr *Session
		if sess, err := s.getSession(sid); err == nil {
			sessPtr = &sess
		}
		s.broadcast(LivePacket{Type: "event", Event: &ev, Session: sessPtr})
	}
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
	argsJSON := RedactAny(detail)

	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	if err := upsertSessionExec(tx, sid, "lrmcp-daemon", "lrmcp", "system", "System Events", "active", now, 0, 0); err != nil {
		_ = tx.Rollback()
		return
	}
	res, err := tx.Exec(`
		INSERT INTO events (session_id, agent_id, wire, kind, tool_name, summary, args_json, result_json, status, created_at)
		VALUES (?, 'lrmcp-daemon', 'system', ?, '', ?, ?, '{}', 'info', ?)
	`, sid, kind, summary, argsJSON, now)
	if err != nil {
		_ = tx.Rollback()
		return
	}
	id, _ := res.LastInsertId()
	insertFTSEntry(tx, id, summary, "", argsJSON, "{}", "", "lrmcp-daemon")
	if err := tx.Commit(); err != nil {
		return
	}
	if s.hasSubscribers() {
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
		var sessPtr *Session
		if sess, err := s.getSession(sid); err == nil {
			sessPtr = &sess
		}
		s.broadcast(LivePacket{Type: "event", Event: &ev, Session: sessPtr})
	}
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
	nowMS := time.Now().UnixMilli()
	out := make([]Session, 0)
	for rows.Next() {
		var item Session
		if err := rows.Scan(&item.ID, &item.AgentID, &item.ClientName, &item.Wire, &item.Title, &item.Status, &item.ToolCount, &item.ErrorCount, &item.StartedAt, &item.LastSeenAt, &item.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, normalizeSessionStatus(item, nowMS))
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

// DeleteAgent permanently removes all sessions, events, FTS5 entries, and
// unreferenced image blobs belonging to agentID.
func (s *Store) DeleteAgent(agentID string) (DeleteResult, error) {
	return s.Delete(DeleteRequest{AgentIDs: []string{agentID}})
}

// DeleteSession permanently removes one session, its events, FTS5 entries, and
// unreferenced image blobs.
func (s *Store) DeleteSession(sessionID string) (DeleteResult, error) {
	return s.Delete(DeleteRequest{SessionIDs: []string{sessionID}})
}

// Delete atomically removes matching agent timelines and/or sessions along with
// their FTS5 index entries and unreferenced blobs, optionally compacting SQLite.
func (s *Store) Delete(req DeleteRequest) (DeleteResult, error) {
	if s == nil || s.db == nil {
		return DeleteResult{}, nil
	}
	agents := dedupeTrimmed(req.AgentIDs)
	sessions := dedupeTrimmed(req.SessionIDs)
	if len(agents) == 0 && len(sessions) == 0 {
		return DeleteResult{}, errors.New("at least one agent_id or session_id is required")
	}
	for _, a := range agents {
		if a == "lrmcp-daemon" {
			return DeleteResult{}, errors.New("cannot delete system daemon timeline")
		}
	}
	for _, sid := range sessions {
		if sid == "system:daemon" {
			return DeleteResult{}, errors.New("cannot delete system daemon timeline")
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return DeleteResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	candidateBlobs := map[string]struct{}{}
	collectBlobs := func(query, arg string) error {
		rows, qErr := tx.Query(query, arg)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			if h != "" {
				candidateBlobs[h] = struct{}{}
			}
		}
		return rows.Err()
	}

	for _, a := range agents {
		if err := collectBlobs(`SELECT DISTINCT image_blob FROM events WHERE agent_id = ? AND image_blob != ''`, a); err != nil {
			return DeleteResult{}, err
		}
	}
	for _, sid := range sessions {
		if err := collectBlobs(`SELECT DISTINCT image_blob FROM events WHERE session_id = ? AND image_blob != ''`, sid); err != nil {
			return DeleteResult{}, err
		}
	}

	var out DeleteResult
	for _, a := range agents {
		if _, err := tx.Exec(`DELETE FROM events_fts WHERE rowid IN (SELECT id FROM events WHERE agent_id = ?)`, a); err != nil {
			return DeleteResult{}, err
		}
		resEv, err := tx.Exec(`DELETE FROM events WHERE agent_id = ?`, a)
		if err != nil {
			return DeleteResult{}, err
		}
		if n, _ := resEv.RowsAffected(); n > 0 {
			out.Events += n
		}
		resSess, err := tx.Exec(`DELETE FROM sessions WHERE agent_id = ?`, a)
		if err != nil {
			return DeleteResult{}, err
		}
		if n, _ := resSess.RowsAffected(); n > 0 {
			out.Sessions += n
		}
	}
	for _, sid := range sessions {
		if _, err := tx.Exec(`DELETE FROM events_fts WHERE rowid IN (SELECT id FROM events WHERE session_id = ?)`, sid); err != nil {
			return DeleteResult{}, err
		}
		resEv, err := tx.Exec(`DELETE FROM events WHERE session_id = ?`, sid)
		if err != nil {
			return DeleteResult{}, err
		}
		if n, _ := resEv.RowsAffected(); n > 0 {
			out.Events += n
		}
		resSess, err := tx.Exec(`DELETE FROM sessions WHERE id = ?`, sid)
		if err != nil {
			return DeleteResult{}, err
		}
		if n, _ := resSess.RowsAffected(); n > 0 {
			out.Sessions += n
		}
	}
	if err := tx.Commit(); err != nil {
		return DeleteResult{}, err
	}

	// Evict any matching in-flight entries so late completions cannot resurrect FTS rows.
	agentSet := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentSet[a] = true
	}
	sessionSet := make(map[string]bool, len(sessions))
	for _, sid := range sessions {
		sessionSet[sid] = true
	}
	s.inFlightMu.Lock()
	for id, meta := range s.inFlight {
		if agentSet[meta.agentID] || sessionSet[meta.sessionID] {
			delete(s.inFlight, id)
		}
	}
	s.inFlightMu.Unlock()

	// Immediately reclaim blobs that were only referenced by the purged events.
	for h := range candidateBlobs {
		if !validBlobHash.MatchString(h) {
			continue
		}
		var dummy int
		if err := s.db.QueryRow(`SELECT 1 FROM events WHERE image_blob = ? LIMIT 1`, h).Scan(&dummy); errors.Is(err, sql.ErrNoRows) {
			if s.blobDir != "" {
				if os.Remove(filepath.Join(s.blobDir, h)) == nil {
					out.Blobs++
				}
			} else {
				s.blobMu.Lock()
				if _, ok := s.memBlobs[h]; ok {
					delete(s.memBlobs, h)
					out.Blobs++
				}
				s.blobMu.Unlock()
			}
		}
	}

	if s.dbPath != "" {
		if req.Vacuum {
			_, prunedBlobs, _ := s.Prune()
			out.Blobs += prunedBlobs
			_, _ = s.db.Exec(`VACUUM;`)
			_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE);`)
		} else if out.Sessions > 0 || out.Events > 0 {
			_, _ = s.db.Exec(`PRAGMA incremental_vacuum;`)
			_, _ = s.db.Exec(`PRAGMA wal_checkpoint(PASSIVE);`)
		}
	}

	if (out.Sessions > 0 || out.Events > 0 || req.Vacuum) && s.hasSubscribers() {
		s.broadcast(LivePacket{Type: "refresh"})
	}
	return out, nil
}

func dedupeTrimmed(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
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
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if info, err := os.Stat(s.dbPath + suffix); err == nil {
				st.DBBytes += info.Size()
			}
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

func (s *Store) pruneLoop() {
	defer close(s.pruneDone)
	// Run one quick pass shortly after startup to reap old events & orphan blobs.
	select {
	case <-time.After(2 * time.Second):
		_, _, _ = s.Prune()
	case <-s.stopPrune:
		return
	}
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_, _, _ = s.Prune()
		case <-s.stopPrune:
			return
		}
	}
}

// Prune enforces event retention limits, compacts FTS5/freelist pages, and
// garbage-collects unreferenced image blobs older than orphanBlobGrace.
func (s *Store) Prune() (deletedEvents int64, deletedBlobs int, err error) {
	if s == nil || s.db == nil {
		return 0, 0, nil
	}
	cutoff := time.Now().Add(-maxEventAge).UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	res1, err := tx.Exec(`DELETE FROM events WHERE created_at < ?`, cutoff)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}
	n1, _ := res1.RowsAffected()
	res2, err := tx.Exec(`DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?)`, maxRetainedEvents)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}
	n2, _ := res2.RowsAffected()
	deletedEvents = n1 + n2
	if deletedEvents > 0 {
		_, _ = tx.Exec(`DELETE FROM events_fts WHERE rowid NOT IN (SELECT id FROM events)`)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	if deletedEvents > 0 && s.dbPath != "" {
		_, _ = s.db.Exec(`PRAGMA incremental_vacuum(200);`)
	}

	// Garbage-collect orphaned image blobs on disk.
	if s.blobDir != "" {
		entries, readErr := os.ReadDir(s.blobDir)
		if readErr == nil && len(entries) > 0 {
			referenced := map[string]bool{}
			rows, qErr := s.db.Query(`SELECT DISTINCT image_blob FROM events WHERE image_blob != ''`)
			if qErr == nil {
				for rows.Next() {
					var h string
					if rows.Scan(&h) == nil && h != "" {
						referenced[h] = true
					}
				}
				_ = rows.Close()
				now := time.Now()
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					name := entry.Name()
					if !validBlobHash.MatchString(name) || referenced[name] {
						continue
					}
					info, statErr := entry.Info()
					if statErr == nil && now.Sub(info.ModTime()) < orphanBlobGrace {
						continue
					}
					if os.Remove(filepath.Join(s.blobDir, name)) == nil {
						deletedBlobs++
					}
				}
			}
		}
	}
	return deletedEvents, deletedBlobs, nil
}

// Close closes subscribers, stops background pruning, checkpoints WAL, and closes SQLite cleanly.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.subMu.Lock()
	alreadyClosed := s.closed
	if !s.closed {
		s.closed = true
		for ch := range s.subscribers {
			close(ch)
			delete(s.subscribers, ch)
		}
	}
	s.subMu.Unlock()
	if !alreadyClosed && s.stopPrune != nil {
		close(s.stopPrune)
		<-s.pruneDone
	}
	if s.db != nil {
		if s.dbPath != "" {
			_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE);`)
			_, _ = s.db.Exec(`PRAGMA optimize;`)
		}
		return s.db.Close()
	}
	return nil
}
