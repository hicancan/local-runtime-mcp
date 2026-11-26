package audit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteWALStoreRedactionFTS5BlobsAndLiveStream(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "lrmcp-audit.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ch, unsub := store.Subscribe()
	defer unsub()

	rawArgs := json.RawMessage(`{"program":"pwsh","args":["-Command","echo hello"],"environment":{"NORMAL":"ok","GITHUB_API_KEY":"super-secret-value","LOCAL_RUNTIME_MCP_LOCAL_TOKEN":"secret-token"}}`)
	evID, sid := store.StartTool(StartToolInput{
		SessionID:  "cf:astra-1",
		AgentID:    "astra-1-mooc",
		ClientName: "dot-cloud",
		Wire:       "cloudflare",
		ToolName:   "process_run",
		RawArgs:    rawArgs,
	})
	if evID <= 0 || sid != "cf:astra-1" {
		t.Fatalf("unexpected start: id=%d sid=%q", evID, sid)
	}

	// Receive live start packet.
	select {
	case pkt := <-ch:
		if pkt.Event == nil || pkt.Event.Status != "running" || strings.Contains(pkt.Event.ArgsJSON, "super-secret-value") || strings.Contains(pkt.Event.ArgsJSON, "secret-token") {
			t.Fatalf("redaction or live start failed: %+v", pkt.Event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing live start event")
	}

	fakePNG := []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4}
	store.FinishTool(evID, sid, FinishToolInput{
		Status:     "ok",
		Result:     map[string]any{"stdout": "hello world", "api_key": "must-not-leak"},
		ImageData:  fakePNG,
		ImageMIME:  "image/png",
		DurationMS: 42,
	})

	// Receive live finish packet.
	var blobHash string
	select {
	case pkt := <-ch:
		if pkt.Event == nil || pkt.Event.Status != "ok" || pkt.Event.DurationMS != 42 || pkt.Event.ImageBlob == "" || strings.Contains(pkt.Event.ResultJSON, "must-not-leak") {
			t.Fatalf("finish event invalid: %+v", pkt.Event)
		}
		blobHash = pkt.Event.ImageBlob
	case <-time.After(2 * time.Second):
		t.Fatal("missing live finish event")
	}

	gotBlob, err := store.GetBlob(blobHash)
	if err != nil || !bytes.Equal(gotBlob, fakePNG) {
		t.Fatalf("blob mismatch: %v %x", err, gotBlob)
	}

	// Record a progress note (agent_report).
	note, err := store.RecordNote(NoteInput{
		SessionID: sid,
		AgentID:   "astra-1-mooc",
		Wire:      "cloudflare",
		Title:     "核对网络技术第六周测验",
		Message:   "已完成 MOOC 页面拉取，正在交叉验证截止时间。",
		Status:    "working",
	})
	if err != nil || note.Kind != "note" {
		t.Fatalf("note failed: %v %+v", err, note)
	}

	// FTS5 search across tool calls and notes.
	hits, err := store.Search("交叉验证", 20)
	if err != nil || len(hits) != 1 || hits[0].ID != note.ID {
		t.Fatalf("FTS5 search failed: %v %+v", err, hits)
	}
	hits2, err := store.Search("hello", 20)
	if err != nil || len(hits2) != 1 || hits2[0].ID != evID {
		t.Fatalf("FTS5 search for tool output failed: %v %+v", err, hits2)
	}

	// Sessions listing preserves title and counts.
	sessions, err := store.ListSessions(10)
	if err != nil || len(sessions) != 1 || sessions[0].Title != "核对网络技术第六周测验" || sessions[0].ToolCount != 1 {
		t.Fatalf("unexpected sessions: %v %+v", err, sessions)
	}

	// Compact list & search omit args_json/result_json; GetEvent + BlobPath recover full payload.
	compactList, err := store.ListEvents(EventFilter{SessionID: sid, Compact: true, Limit: 20})
	if err != nil || len(compactList) != 2 || compactList[0].ArgsJSON != "" || compactList[0].ResultJSON != "" {
		t.Fatalf("compact list failed: %v %+v", err, compactList)
	}
	compactHits, err := store.SearchWithOptions("hello", 20, true)
	if err != nil || len(compactHits) != 1 || compactHits[0].ArgsJSON != "" || compactHits[0].ResultJSON != "" {
		t.Fatalf("compact search failed: %v %+v", err, compactHits)
	}
	fullEv, err := store.GetEvent(evID)
	if err != nil || fullEv.ID != evID || fullEv.ArgsJSON == "" || fullEv.ResultJSON == "" {
		t.Fatalf("GetEvent failed: %v %+v", err, fullEv)
	}
	if store.BlobPath(blobHash) == "" {
		t.Fatalf("expected non-empty blob path for %s", blobHash)
	}

	// JSONL export produces valid lines.
	var exported bytes.Buffer
	if err := store.ExportJSONL(&exported, sid); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(exported.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d: %s", len(lines), exported.String())
	}
}

func TestAuditSessionDoneGuardIsolationAndOrphanBlobGC(t *testing.T) {
	// Isolated in-memory stores must not share state.
	mem1, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer mem1.Close()
	mem2, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer mem2.Close()
	_, _ = mem1.RecordNote(NoteInput{SessionID: "local:iso", AgentID: "a1", Wire: "local", Title: "only in mem1", Message: "isolated"})
	if list2, err := mem2.ListSessions(10); err != nil || len(list2) != 0 {
		t.Fatalf("in-memory stores leaked across Open(\"\"): %+v err=%v", list2, err)
	}

	// FinishTool must never resurrect a session already marked done.
	dbPath := filepath.Join(t.TempDir(), "audit-gc.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	evID, sid := store.StartTool(StartToolInput{
		SessionID: "local:task-done-guard",
		AgentID:   "agy",
		Wire:      "local",
		ToolName:  "run",
		RawArgs:   json.RawMessage(`{"title":"测试任务"}`),
	})
	if _, err := store.RecordNote(NoteInput{
		SessionID: sid,
		AgentID:   "agy",
		Wire:      "local",
		Message:   "结束",
		Status:    "done",
	}); err != nil {
		t.Fatal(err)
	}
	store.FinishTool(evID, sid, FinishToolInput{Status: "ok", Result: map[string]any{"ok": true}})
	sessions, err := store.ListSessions(10)
	if err != nil || len(sessions) != 1 || sessions[0].Status != "done" {
		t.Fatalf("FinishTool overwrote done session status: %+v err=%v", sessions, err)
	}

	// Orphan blob older than grace period must be pruned while referenced blob survives.
	keepBlob := []byte{0x89, 'P', 'N', 'G', 9, 9, 9}
	evKeep, _ := store.StartTool(StartToolInput{SessionID: sid, AgentID: "agy", Wire: "local", ToolName: "run"})
	store.FinishTool(evKeep, sid, FinishToolInput{Status: "ok", ImageData: keepBlob, ImageMIME: "image/png"})
	orphanHash := strings.Repeat("a", 64)
	orphanPath := filepath.Join(store.blobDir, orphanHash)
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	_ = os.Chtimes(orphanPath, oldTime, oldTime)

	_, delBlobs, err := store.Prune()
	if err != nil || delBlobs != 1 {
		t.Fatalf("expected 1 orphan blob deleted, got %d err=%v", delBlobs, err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan blob should have been removed")
	}
}

func TestDeleteAgentAndSessionPurgesEventsFTSBlobsAndNotifies(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit-delete.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	uniquePNG := []byte{0x89, 'P', 'N', 'G', 10, 20, 30, 40}
	sharedPNG := []byte{0x89, 'P', 'N', 'G', 50, 60, 70, 80}

	evReal, _ := store.StartTool(StartToolInput{
		SessionID: "local:real-s1",
		AgentID:   "real-client",
		Wire:      "local",
		ToolName:  "run",
		RawArgs:   json.RawMessage(`{"title":"keep-real-token"}`),
	})
	store.FinishTool(evReal, "local:real-s1", FinishToolInput{
		Status:    "ok",
		Result:    map[string]any{"msg": "keep-real-token"},
		ImageData: sharedPNG,
		ImageMIME: "image/png",
	})

	evSmoke1, _ := store.StartTool(StartToolInput{
		SessionID: "local:smoke-s1",
		AgentID:   "smoke-agent",
		Wire:      "local",
		ToolName:  "run",
		RawArgs:   json.RawMessage(`{"title":"purge-smoke-token"}`),
	})
	store.FinishTool(evSmoke1, "local:smoke-s1", FinishToolInput{
		Status:    "ok",
		Result:    map[string]any{"msg": "purge-smoke-token"},
		ImageData: uniquePNG,
		ImageMIME: "image/png",
	})
	evSmokeShared, _ := store.StartTool(StartToolInput{
		SessionID: "local:smoke-s2",
		AgentID:   "smoke-agent",
		Wire:      "local",
		ToolName:  "run",
	})
	store.FinishTool(evSmokeShared, "local:smoke-s2", FinishToolInput{
		Status:    "ok",
		ImageData: sharedPNG,
		ImageMIME: "image/png",
	})
	_, _ = store.RecordNote(NoteInput{
		SessionID: "local:partial-drop",
		AgentID:   "partial-client",
		Wire:      "local",
		Message:   "drop-session-token",
	})
	_, _ = store.RecordNote(NoteInput{
		SessionID: "local:partial-keep",
		AgentID:   "partial-client",
		Wire:      "local",
		Message:   "keep-session-token",
	})
	store.RecordSystem("system", "daemon started", nil)

	ev1Full, err := store.GetEvent(evSmoke1)
	if err != nil || ev1Full.ImageBlob == "" {
		t.Fatalf("missing unique blob: %+v %v", ev1Full, err)
	}
	evRealFull, err := store.GetEvent(evReal)
	if err != nil || evRealFull.ImageBlob == "" {
		t.Fatalf("missing shared blob: %+v %v", evRealFull, err)
	}

	// Guard rails: empty request and system daemon timeline must be rejected.
	if _, err := store.Delete(DeleteRequest{}); err == nil {
		t.Fatal("expected error for empty delete request")
	}
	if _, err := store.DeleteAgent("lrmcp-daemon"); err == nil {
		t.Fatal("expected error deleting lrmcp-daemon")
	}
	if _, err := store.DeleteSession("system:daemon"); err == nil {
		t.Fatal("expected error deleting system:daemon")
	}

	ch, unsub := store.Subscribe()
	defer unsub()

	res, err := store.Delete(DeleteRequest{
		AgentIDs:   []string{"smoke-agent"},
		SessionIDs: []string{"local:partial-drop"},
		Vacuum:     true,
	})
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if res.Sessions != 3 || res.Events != 3 || res.Blobs != 1 {
		t.Fatalf("unexpected DeleteResult: %+v", res)
	}

	// SSE refresh notification must fire.
	select {
	case pkt := <-ch:
		if pkt.Type != "refresh" {
			t.Fatalf("expected refresh packet, got %+v", pkt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing SSE refresh packet")
	}

	// Unique blob must be removed from disk; shared blob must survive.
	if store.BlobPath(ev1Full.ImageBlob) != "" {
		t.Fatalf("unique blob %s was not removed", ev1Full.ImageBlob)
	}
	if store.BlobPath(evRealFull.ImageBlob) == "" {
		t.Fatalf("shared blob %s was wrongly removed", evRealFull.ImageBlob)
	}

	// FTS5 index must be clean for deleted records and intact for kept records.
	if hits, err := store.Search("purge-smoke-token", 10); err != nil || len(hits) != 0 {
		t.Fatalf("deleted agent still searchable: %+v err=%v", hits, err)
	}
	if hits, err := store.Search("drop-session-token", 10); err != nil || len(hits) != 0 {
		t.Fatalf("deleted session still searchable: %+v err=%v", hits, err)
	}
	if hits, err := store.Search("keep-real-token", 10); err != nil || len(hits) != 1 {
		t.Fatalf("kept agent search broken: %+v err=%v", hits, err)
	}
	if hits, err := store.Search("keep-session-token", 10); err != nil || len(hits) != 1 {
		t.Fatalf("kept session search broken: %+v err=%v", hits, err)
	}

	// In-flight tool completion after purge must not resurrect deleted events or FTS rows.
	inFlightID, _ := store.StartTool(StartToolInput{
		SessionID: "local:ephemeral-s1",
		AgentID:   "ephemeral-agent",
		Wire:      "local",
		ToolName:  "run",
	})
	if _, err := store.DeleteAgent("ephemeral-agent"); err != nil {
		t.Fatal(err)
	}
	store.FinishTool(inFlightID, "local:ephemeral-s1", FinishToolInput{
		Status: "ok",
		Result: map[string]any{"ghost": "must-not-resurrect"},
	})
	if hits, err := store.Search("must-not-resurrect", 10); err != nil || len(hits) != 0 {
		t.Fatalf("deleted in-flight event resurrected in FTS: %+v err=%v", hits, err)
	}
}
