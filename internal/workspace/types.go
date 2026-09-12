package workspace

// Info describes a workspace and its detected capabilities.
type Info struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Exists       bool            `json:"exists"`
	Capabilities map[string]bool `json:"capabilities"`
}

// TreeEntry describes a filesystem entry relative to a workspace.
type TreeEntry struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"modified,omitempty"`
}

// ReadResult contains UTF-8 text and metadata.
type ReadResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Truncated bool   `json:"truncated"`
}

// FileInfo describes one filesystem entry without returning its contents.
type FileInfo struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	Size     int64  `json:"size,omitempty"`
	ModTime  string `json:"modified"`
	MIMEType string `json:"mime_type,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

// ImageReadResult describes an image returned as MCP image content.
type ImageReadResult struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MIMEType string `json:"mime_type"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	SHA256   string `json:"sha256"`
}

// WriteResult describes a completed full-file write.
type WriteResult struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// SearchMatch is one matching source line.
type SearchMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

// CommandResult is the structured result of a CLI or Git command.
type CommandResult struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out,omitempty"`
}
