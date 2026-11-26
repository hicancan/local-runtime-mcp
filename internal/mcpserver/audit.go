package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ActiveMCPSession describes one currently connected MCP session that can be
// inspected or kicked by the owner.
type ActiveMCPSession struct {
	ID         string `json:"id"`
	ClientName string `json:"client_name,omitempty"`
	Wire       string `json:"wire"`
}

type sessionInput struct {
	Action    string `json:"action" jsonschema:"session lifecycle action: start (open fresh named task thread), update (post progress milestone), or done (finish task and reclaim temporary browser tabs)"`
	Title     string `json:"title,omitempty" jsonschema:"concise task title shown in the activity sidebar (recommended on start)"`
	Message   string `json:"message,omitempty" jsonschema:"concise human-readable milestone or completion summary"`
	Status    string `json:"status,omitempty" jsonschema:"optional status hint: working, waiting, done, or error"`
	SessionID string `json:"session_id,omitempty" jsonschema:"optional explicit task session identifier"`
	AgentID   string `json:"agent_id,omitempty" jsonschema:"optional logical agent identifier when multiple sub-agents share one connection"`
}

type sessionOutput struct {
	Recorded   bool   `json:"recorded"`
	EventID    int64  `json:"event_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Status     string `json:"status,omitempty"`
	ClosedTabs *int   `json:"closed_tabs,omitempty"`
}

func shortTaskID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Managed) getActiveTask(baseKey string) string {
	if m == nil || baseKey == "" {
		return ""
	}
	m.taskMu.RLock()
	defer m.taskMu.RUnlock()
	return m.activeTasks[baseKey]
}

func (m *Managed) setActiveTask(baseKey, taskSID string) {
	if m == nil || baseKey == "" || taskSID == "" {
		return
	}
	m.taskMu.Lock()
	if m.activeTasks == nil {
		m.activeTasks = map[string]string{}
	}
	if _, exists := m.activeTasks[baseKey]; !exists {
		m.taskOrder = append(m.taskOrder, baseKey)
	}
	m.activeTasks[baseKey] = taskSID
	for len(m.activeTasks) > 256 && len(m.taskOrder) > 0 {
		oldest := m.taskOrder[0]
		m.taskOrder = m.taskOrder[1:]
		if oldest != baseKey {
			delete(m.activeTasks, oldest)
		}
	}
	m.taskMu.Unlock()
}

func (m *Managed) clearActiveTask(keyOrTaskSID string) {
	if m == nil || keyOrTaskSID == "" {
		return
	}
	m.taskMu.Lock()
	delete(m.activeTasks, keyOrTaskSID)
	for k, v := range m.activeTasks {
		if v == keyOrTaskSID {
			delete(m.activeTasks, k)
		}
	}
	if len(m.taskOrder) > 0 {
		kept := m.taskOrder[:0]
		for _, k := range m.taskOrder {
			if _, ok := m.activeTasks[k]; ok {
				kept = append(kept, k)
			}
		}
		m.taskOrder = kept
	}
	m.taskMu.Unlock()
}

func sessionToolSpec(m *Managed, store *audit.Store, defaultWire string) toolSpec {
	sessTool := inputTool[sessionInput](
		tool(
			"session",
			"Manage task session & milestones",
			"Optional for one-off read-only queries (call run directly instead). Use action='start' with title for multi-step tasks or browser workflows, action='update' for major milestones, and action='done' when finished to close the task thread and reclaim temporary browser tabs not marked via markHandoff.",
			true, false, false, false,
		),
		func(schema *jsonschema.Schema) {
			schema.Required = []string{"action"}
			schema.Properties["action"].Enum = enum("start", "update", "done")
			schema.Properties["status"].Enum = enum("working", "waiting", "done", "error")
		},
	)
	return toolSpec{
		name:     "session",
		readOnly: true,
		register: func(s *mcp.Server) {
			mcp.AddTool(s, sessTool, func(ctx context.Context, req *mcp.CallToolRequest, in sessionInput) (*mcp.CallToolResult, sessionOutput, error) {
				action := strings.ToLower(strings.TrimSpace(in.Action))
				switch action {
				case "start", "update", "done":
				default:
					return nil, sessionOutput{}, errors.New("action must be start, update, or done")
				}

				sid, aid, clientName, wire, baseKey := resolveCaller(ctx, m, req.Session, defaultWire, in.AgentID)

				explicitSID := audit.SanitizeID(in.SessionID)
				if explicitSID != "" {
					if strings.Contains(explicitSID, ":") {
						sid = explicitSID
					} else {
						sid = wire + ":" + explicitSID
					}
					if baseKey != "" && action != "done" {
						m.setActiveTask(baseKey, sid)
					}
				} else if action == "start" {
					sid = wire + ":task-" + shortTaskID()
					if baseKey != "" {
						m.setActiveTask(baseKey, sid)
					}
				}

				status := strings.TrimSpace(in.Status)
				if status == "" {
					switch action {
					case "done":
						status = "done"
					default:
						status = "working"
					}
				}
				msg := strings.TrimSpace(in.Message)
				if msg == "" {
					if t := strings.TrimSpace(in.Title); t != "" {
						msg = t
					} else {
						msg = "session " + action
					}
				}

				var closedTabs *int
				if action == "done" {
					m.clearActiveTask(sid)
					if baseKey != "" {
						m.clearActiveTask(baseKey)
					}
					n := 0
					if m.runtime != nil {
						n = m.runtime.CloseSession(ctx, sid)
					}
					closedTabs = &n
				}

				if store == nil {
					return nil, sessionOutput{Recorded: true, SessionID: sid, Status: status, ClosedTabs: closedTabs}, nil
				}
				ev, err := store.RecordNote(audit.NoteInput{
					SessionID:  sid,
					AgentID:    aid,
					ClientName: clientName,
					Wire:       wire,
					Title:      in.Title,
					Message:    msg,
					Status:     status,
				})
				if err != nil {
					return nil, sessionOutput{}, err
				}
				return nil, sessionOutput{Recorded: true, EventID: ev.ID, SessionID: ev.SessionID, Status: status, ClosedTabs: closedTabs}, nil
			})
		},
	}
}

func resolveCaller(ctx context.Context, m *Managed, session *mcp.ServerSession, defaultWire, explicitAgent string) (sessionID, agentID, clientName, wire, baseKey string) {
	meta := audit.CallMetaFromContext(ctx)
	wire = meta.Wire
	if wire == "" {
		wire = defaultWire
	}
	if wire == "" {
		wire = "local"
	}
	if session != nil {
		if init := session.InitializeParams(); init != nil && init.ClientInfo != nil {
			clientName = strings.TrimSpace(init.ClientInfo.Name)
		}
	}
	if meta.ClientName != "" && clientName == "" {
		clientName = meta.ClientName
	}

	if explicit := audit.SanitizeID(explicitAgent); explicit != "" {
		agentID = explicit
	} else if meta.AgentID != "" {
		agentID = meta.AgentID
	} else if clientName != "" {
		agentID = clientName
	} else {
		agentID = wire
	}

	isStatelessWire := wire == "http" || wire == "cloudflare" || wire == "openai"

	if !isStatelessWire {
		switch {
		case meta.SessionID != "":
			baseKey = meta.SessionID
		case session != nil && session.ID() != "":
			baseKey = wire + ":" + session.ID()
		case session != nil:
			baseKey = fmt.Sprintf("%s:conn-%p", wire, session)
		}
		if baseKey != "" {
			if activeTask := m.getActiveTask(baseKey); activeTask != "" {
				return activeTask, agentID, clientName, wire, baseKey
			}
		}
	}

	switch {
	case meta.SessionID != "":
		sessionID = meta.SessionID
		if explicit := audit.SanitizeID(explicitAgent); explicit != "" && !strings.Contains(sessionID, explicit) && isStatelessWire && !strings.Contains(meta.SessionID, ":") {
			sessionID = wire + ":" + explicit
		}
	case isStatelessWire:
		sessionID = wire + ":" + agentID
	case session != nil && session.ID() != "":
		sessionID = wire + ":" + session.ID()
	default:
		sessionID = wire + ":" + agentID
	}
	return sessionID, agentID, clientName, wire, baseKey
}

func (m *Managed) installMiddleware(store *audit.Store, defaultWire string) {
	m.Server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			meta := audit.CallMetaFromContext(ctx)

			if method == "tools/list" {
				res, err := next(ctx, method, req)
				if err != nil || meta.Policy == nil {
					return res, err
				}
				listed, ok := res.(*mcp.ListToolsResult)
				if !ok || listed == nil {
					return res, err
				}
				filtered := make([]*mcp.Tool, 0, len(listed.Tools))
				for _, t := range listed.Tools {
					ro := t.Annotations != nil && t.Annotations.ReadOnlyHint
					dest := t.Annotations != nil && t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint
					if meta.Policy(t.Name, ro, dest) {
						filtered = append(filtered, t)
					}
				}
				cloned := *listed
				cloned.Tools = filtered
				return &cloned, nil
			}

			if method != "tools/call" {
				return next(ctx, method, req)
			}

			rawParams, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok || rawParams == nil {
				return next(ctx, method, req)
			}
			toolName := rawParams.Name
			var ss *mcp.ServerSession
			if s, ok := req.GetSession().(*mcp.ServerSession); ok {
				ss = s
			}
			sid, aid, clientName, wire, _ := resolveCaller(ctx, m, ss, defaultWire, "")

			if meta.Policy != nil {
				ro, dest, known := m.toolHints(toolName)
				if !known || !meta.Policy(toolName, ro, dest) {
					errMsg := "tool " + toolName + " is not granted to this remote token"
					if store != nil {
						evID, auditSID := store.StartTool(audit.StartToolInput{
							SessionID:  sid,
							AgentID:    aid,
							ClientName: clientName,
							Wire:       wire,
							ToolName:   toolName,
							RawArgs:    rawParams.Arguments,
						})
						store.FinishTool(evID, auditSID, audit.FinishToolInput{
							Status:    "error",
							ErrorText: errMsg,
						})
					}
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: errMsg}},
					}, nil
				}
			}

			if toolName == "session" || store == nil {
				return next(ctx, method, req)
			}

			// Extract optional done_title and explicit session_id for accurate audit attribution on `run`.
			var doneTitle string
			if toolName == "run" && len(rawParams.Arguments) > 0 {
				var peek struct {
					DoneTitle string `json:"done_title"`
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal(rawParams.Arguments, &peek) == nil {
					doneTitle = strings.TrimSpace(peek.DoneTitle)
					if explicit := audit.SanitizeID(peek.SessionID); explicit != "" {
						if strings.Contains(explicit, ":") {
							sid = explicit
						} else {
							sid = wire + ":" + explicit
						}
					}
				}
			}

			start := time.Now()
			auditArgs := rawParams.Arguments
			if toolName == "run" {
				auditArgs = redactInboundArgs(auditArgs)
			}
			evID, auditSID := store.StartTool(audit.StartToolInput{
				SessionID:  sid,
				AgentID:    aid,
				ClientName: clientName,
				Wire:       wire,
				ToolName:   toolName,
				RawArgs:    auditArgs,
			})

			res, err := next(ctx, method, req)
			durMS := time.Since(start).Milliseconds()

			finish := audit.FinishToolInput{
				Status:     "ok",
				DurationMS: durMS,
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					finish.Status = "canceled"
				} else {
					finish.Status = "error"
				}
				finish.ErrorText = err.Error()
			} else if callRes, ok := res.(*mcp.CallToolResult); ok && callRes != nil {
				finish.Result, finish.ImageData, finish.ImageMIME, finish.ErrorText = extractCallResult(callRes)
				if callRes.IsError {
					finish.Status = "error"
				}
			}
			if finish.Status == "ok" && doneTitle != "" {
				finish.FinalSummary = doneTitle
			}
			store.FinishTool(evID, auditSID, finish)
			return res, err
		}
	})
}

func (m *Managed) toolHints(name string) (readOnly, destructive, known bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, spec := range m.static {
		if spec.name == name {
			return spec.readOnly, spec.destructive, true
		}
	}
	if m.registry != nil {
		for _, def := range m.registry.List() {
			if def.Name == name {
				return def.Annotations.ReadOnlyHint, def.Annotations.DestructiveHint, true
			}
		}
		for _, def := range m.registry.AppDefinitions() {
			if def.Name == name {
				return def.Annotations.ReadOnlyHint, def.Annotations.DestructiveHint, true
			}
		}
	}
	return false, true, false
}

func extractCallResult(res *mcp.CallToolResult) (structured any, img []byte, mime string, errText string) {
	var texts []string
	for _, content := range res.Content {
		switch v := content.(type) {
		case *mcp.TextContent:
			if v.Text != "" {
				texts = append(texts, v.Text)
			}
		case *mcp.ImageContent:
			if len(img) == 0 && len(v.Data) > 0 {
				img = v.Data
				mime = v.MIMEType
			}
		}
	}
	if res.IsError {
		errText = strings.Join(texts, "\n")
	}
	if res.StructuredContent != nil {
		structured = res.StructuredContent
		return structured, img, mime, errText
	}
	if len(texts) > 0 {
		joined := strings.Join(texts, "\n")
		var parsed any
		if json.Unmarshal([]byte(joined), &parsed) == nil {
			structured = parsed
		} else {
			structured = map[string]any{"text": joined}
		}
	} else if len(img) > 0 {
		structured = map[string]any{"image_mime": mime, "image_bytes": len(img)}
	}
	return structured, img, mime, errText
}

// ActiveSessions returns currently connected MCP sessions on this server.
func (m *Managed) ActiveSessions() []ActiveMCPSession {
	if m == nil || m.Server == nil {
		return nil
	}
	out := []ActiveMCPSession{}
	for ss := range m.Server.Sessions() {
		id := ss.ID()
		if id == "" {
			continue
		}
		clientName := ""
		if init := ss.InitializeParams(); init != nil && init.ClientInfo != nil {
			clientName = init.ClientInfo.Name
		}
		out = append(out, ActiveMCPSession{ID: id, ClientName: clientName, Wire: m.wire})
	}
	return out
}

// CleanupSession releases the JS VM, temporary browser tabs, and active task mapping
// associated with sessionID when a local client disconnects (without kicking other peers).
func (m *Managed) CleanupSession(sessionID string) {
	if m == nil || sessionID == "" {
		return
	}
	taskSID := m.getActiveTask(sessionID)
	m.clearActiveTask(sessionID)
	if m.runtime != nil {
		m.runtime.CloseSession(context.Background(), sessionID)
		if taskSID != "" && taskSID != sessionID {
			m.runtime.CloseSession(context.Background(), taskSID)
		}
	}
}

// CloseSession closes an active MCP session matching sessionID and releases its stateful JS VM.
func (m *Managed) CloseSession(sessionID string) bool {
	if m == nil || m.Server == nil || sessionID == "" {
		return false
	}
	m.clearActiveTask(sessionID)
	if m.runtime != nil {
		m.runtime.CloseSession(context.Background(), sessionID)
	}
	bare := sessionID
	if idx := strings.IndexByte(sessionID, ':'); idx >= 0 {
		bare = sessionID[idx+1:]
	}
	closed := false
	for ss := range m.Server.Sessions() {
		if ss.ID() == sessionID || ss.ID() == bare {
			_ = ss.Close()
			closed = true
		}
	}
	return closed
}
