package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type agentReportInput struct {
	Message    string `json:"message" jsonschema:"concise human-readable progress milestone or status note shown in the owner's live activity timeline"`
	Title      string `json:"title,omitempty" jsonschema:"optional short task/session title shown in the activity sidebar"`
	AgentID    string `json:"agent_id,omitempty" jsonschema:"optional logical agent identifier when multiple sub-agents share one connection"`
	SessionID  string `json:"session_id,omitempty" jsonschema:"optional explicit task session identifier to group one task's calls into its own thread under this agent"`
	NewSession bool   `json:"new_session,omitempty" jsonschema:"when true on a persistent connection, opens a fresh task session under this agent for this note and subsequent tool calls"`
	Status     string `json:"status,omitempty" jsonschema:"optional session status hint: working, waiting, done, or error"`
}

type agentReportOutput struct {
	Recorded  bool   `json:"recorded"`
	EventID   int64  `json:"event_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
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
	if len(m.activeTasks) > 256 {
		m.activeTasks = map[string]string{}
	}
	m.activeTasks[baseKey] = taskSID
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
	m.taskMu.Unlock()
}

func agentReportToolSpec(m *Managed, store *audit.Store, defaultWire string) toolSpec {
	reportTool := inputTool[agentReportInput](
		tool(
			"agent_report",
			"Report progress note",
			"Post a short, human-readable progress note to the owner's live local activity stream (lrmcp ui). Read-only and zero side effects on files, processes, or browser tabs. Pass new_session=true (or a distinct session_id) with title when starting a brand-new task so each task appears as its own thread under your agent folder.",
			true, false, false, false,
		),
		func(schema *jsonschema.Schema) {
			schema.Properties["status"].Enum = enum("working", "waiting", "done", "error")
		},
	)
	return toolSpec{
		name:     "agent_report",
		readOnly: true,
		register: func(s *mcp.Server) {
			mcp.AddTool(s, reportTool, func(ctx context.Context, req *mcp.CallToolRequest, in agentReportInput) (*mcp.CallToolResult, agentReportOutput, error) {
				msg := strings.TrimSpace(in.Message)
				if msg == "" {
					return nil, agentReportOutput{}, errors.New("message is required")
				}
				if store == nil {
					return nil, agentReportOutput{Recorded: true}, nil
				}
				sid, aid, clientName, wire, baseKey := resolveCaller(ctx, m, req.Session, defaultWire, in.AgentID)

				explicitSID := audit.SanitizeID(in.SessionID)
				if explicitSID != "" {
					if strings.Contains(explicitSID, ":") {
						sid = explicitSID
					} else {
						sid = wire + ":" + explicitSID
					}
					if baseKey != "" {
						m.setActiveTask(baseKey, sid)
					}
				} else if in.NewSession {
					sid = wire + ":task-" + shortTaskID()
					if baseKey != "" {
						m.setActiveTask(baseKey, sid)
					}
				}

				ev, err := store.RecordNote(audit.NoteInput{
					SessionID:  sid,
					AgentID:    aid,
					ClientName: clientName,
					Wire:       wire,
					Title:      in.Title,
					Message:    msg,
					Status:     in.Status,
				})
				if err != nil {
					return nil, agentReportOutput{}, err
				}
				return nil, agentReportOutput{Recorded: true, EventID: ev.ID, SessionID: ev.SessionID}, nil
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
		// Stateless HTTP / tunnel calls without X-LRMCP-Session coalesce by wire + agent identity.
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

			// agent_report writes its own dedicated "note" event; avoid duplicate tool_call rows.
			if toolName == "agent_report" || store == nil {
				return next(ctx, method, req)
			}

			start := time.Now()
			evID, auditSID := store.StartTool(audit.StartToolInput{
				SessionID:  sid,
				AgentID:    aid,
				ClientName: clientName,
				Wire:       wire,
				ToolName:   toolName,
				RawArgs:    rawParams.Arguments,
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

// CloseSession closes an active MCP session matching sessionID (accepting either
// raw MCP session ID, wire-prefixed "<wire>:<id>", or an active task session ID).
func (m *Managed) CloseSession(sessionID string) bool {
	if m == nil || m.Server == nil || sessionID == "" {
		return false
	}
	m.clearActiveTask(sessionID)
	bare := sessionID
	if idx := strings.IndexByte(sessionID, ':'); idx >= 0 {
		bare = sessionID[idx+1:]
	}
	closed := false
	for ss := range m.Server.Sessions() {
		id := ss.ID()
		if id != "" && (id == sessionID || id == bare || m.wire+":"+id == sessionID) {
			_ = ss.Close()
			closed = true
		}
	}
	return closed
}
