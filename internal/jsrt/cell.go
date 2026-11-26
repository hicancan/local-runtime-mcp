package jsrt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

const (
	defaultYieldMS     = 15_000
	maxYieldMS         = 60_000
	defaultMaxOutBytes = 256 << 10
	maxOutBytesLimit   = 4 << 20
	maxImagesPerCell   = 8
	cellRetention      = 10 * time.Minute
)

// ImagePayload is an image explicitly emitted by JS code via emitImage(...)
// to be returned directly as native MCP ImageContent.
type ImagePayload struct {
	Data     []byte `json:"-"`
	MIMEType string `json:"mime_type"`
	Label    string `json:"label,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// InboundFile describes a file received from the MCP host (for example via
// ChatGPT _meta["openai/fileParams"]) and staged locally for this run cell.
type InboundFile struct {
	FileID     string `json:"file_id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	MIMEType   string `json:"mime_type"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	DurationMS int64  `json:"-"`
}

// SubOp records one fine-grained machine operation invoked inside a run cell
// so /ui renders a complete structured execution tree under the run title.
type SubOp struct {
	Op         string `json:"op"`
	Target     string `json:"target,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"` // "ok" | "error"
	Error      string `json:"error,omitempty"`
}

// RunOptions configures one stateful JS execution cell.
type RunOptions struct {
	SessionID      string
	Title          string
	DoneTitle      string
	Code           string
	YieldMS        int
	MaxOutputBytes int
	ReadOnly       bool
	InboundFiles   []InboundFile
}

// WaitOptions configures awaiting, polling, or terminating a running cell.
type WaitOptions struct {
	CellID         string
	YieldMS        int
	Terminate      bool
	SessionID      string
	EnforceSession bool
}

// CellResult is the structured outcome of a run or wait call.
type CellResult struct {
	CellID       string         `json:"cell_id"`
	Status       string         `json:"status"` // "done", "running", "error", "canceled"
	Title        string         `json:"title"`
	DoneTitle    string         `json:"done_title,omitempty"`
	Value        any            `json:"value,omitempty"`
	Output       string         `json:"output,omitempty"`
	Truncated    bool           `json:"truncated,omitempty"`
	Error        string         `json:"error,omitempty"`
	DurationMS   int64          `json:"duration_ms"`
	SubOps       []SubOp        `json:"sub_ops,omitempty"`
	SubOpErrors  int            `json:"sub_op_errors,omitempty"`
	InboundFiles []InboundFile  `json:"inbound_files,omitempty"`
	Images       []ImagePayload `json:"-"`
}

type cellRun struct {
	id           string
	sessionID    string
	title        string
	doneTitle    string
	readOnly     bool
	maxOut       int
	inboundFiles []InboundFile
	startedAt    time.Time
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}

	mu          sync.Mutex
	status      string // "running", "done", "error", "canceled"
	value       any
	errText     string
	finishedAt  time.Time
	output      strings.Builder
	readOffset  int
	truncated   bool
	images      []ImagePayload
	subOps      []SubOp
	subOpOffset int
}

func newCellID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "c_" + hex.EncodeToString(b)
}

func newCellRun(parent context.Context, opts RunOptions) *cellRun {
	maxOut := opts.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = defaultMaxOutBytes
	}
	if maxOut > maxOutBytesLimit {
		maxOut = maxOutBytesLimit
	}
	ctx, cancel := context.WithCancel(parent)
	c := &cellRun{
		id:           newCellID(),
		sessionID:    opts.SessionID,
		title:        opts.Title,
		doneTitle:    opts.DoneTitle,
		readOnly:     opts.ReadOnly,
		maxOut:       maxOut,
		inboundFiles: append([]InboundFile(nil), opts.InboundFiles...),
		startedAt:    time.Now(),
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		status:       "running",
	}
	for _, f := range c.inboundFiles {
		c.recordSubOp("inbound.file", f.Name, f.DurationMS, nil)
	}
	return c
}

func (c *cellRun) appendOutput(s string) {
	if s == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rem := c.maxOut - c.output.Len()
	if rem <= 0 {
		c.truncated = true
		return
	}
	if len(s) > rem {
		c.output.WriteString(truncateUTF8Bytes(s, rem))
		c.truncated = true
		return
	}
	c.output.WriteString(s)
}

func (c *cellRun) addImage(img ImagePayload) bool {
	if len(img.Data) == 0 || img.MIMEType == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.images) >= maxImagesPerCell {
		return false
	}
	c.images = append(c.images, img)
	return true
}

func (c *cellRun) recordSubOp(op, target string, durMS int64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.subOps) >= 200 {
		return
	}
	item := SubOp{
		Op:         op,
		Target:     target,
		DurationMS: durMS,
		Status:     "ok",
	}
	if err != nil {
		item.Status = "error"
		msg := err.Error()
		if len(msg) > 240 {
			msg = truncateUTF8Bytes(msg, 240) + "…"
		}
		item.Error = msg
	}
	c.subOps = append(c.subOps, item)
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) == 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

func (c *cellRun) finish(status string, val any, errText string) {
	c.mu.Lock()
	if c.status != "running" {
		c.mu.Unlock()
		return
	}
	c.status = status
	c.value = val
	c.errText = errText
	c.finishedAt = time.Now()
	c.mu.Unlock()
	c.cancel()
	close(c.done)
}

func (c *cellRun) snapshot(incremental bool) CellResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	fullOut := c.output.String()
	var out string
	if incremental {
		if c.readOffset < len(fullOut) {
			out = fullOut[c.readOffset:]
			c.readOffset = len(fullOut)
		}
	} else {
		out = fullOut
		c.readOffset = len(fullOut)
	}

	var imgs []ImagePayload
	if len(c.images) > 0 {
		imgs = append([]ImagePayload(nil), c.images...)
		c.images = nil
	}

	var ops []SubOp
	if incremental {
		if c.subOpOffset < len(c.subOps) {
			ops = append([]SubOp(nil), c.subOps[c.subOpOffset:]...)
			c.subOpOffset = len(c.subOps)
		}
	} else {
		ops = append([]SubOp(nil), c.subOps...)
		c.subOpOffset = len(c.subOps)
	}

	subOpErrs := 0
	for _, op := range ops {
		if op.Status == "error" {
			subOpErrs++
		}
	}

	end := time.Now()
	if !c.finishedAt.IsZero() {
		end = c.finishedAt
	}

	var inbound []InboundFile
	if !incremental && len(c.inboundFiles) > 0 {
		inbound = append([]InboundFile(nil), c.inboundFiles...)
	}

	return CellResult{
		CellID:       c.id,
		Status:       c.status,
		Title:        c.title,
		DoneTitle:    c.doneTitle,
		Value:        c.value,
		Output:       out,
		Truncated:    c.truncated,
		Error:        c.errText,
		DurationMS:   end.Sub(c.startedAt).Milliseconds(),
		SubOps:       ops,
		SubOpErrors:  subOpErrs,
		InboundFiles: inbound,
		Images:       imgs,
	}
}
