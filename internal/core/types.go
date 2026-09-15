package core

import "time"

type RootInfo struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type FileEntry struct {
	Path    string    `json:"path"`
	Type    string    `json:"type"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"modified_at"`
}

type FileInfo struct {
	Path     string    `json:"path"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"modified_at"`
	MIMEType string    `json:"mime_type,omitempty"`
	SHA256   string    `json:"sha256,omitempty"`
}

type TextReadResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Truncated bool   `json:"truncated"`
}

type TextWriteResult struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
	Created bool   `json:"created"`
}

type TextEditResult struct {
	Path           string `json:"path"`
	Bytes          int    `json:"bytes"`
	SHA256         string `json:"sha256"`
	PreviousSHA256 string `json:"previous_sha256"`
	Replacements   int    `json:"replacements"`
}

type SearchMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

type ImageReadResult struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MIMEType string `json:"mime_type"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	SHA256   string `json:"sha256"`
}

type ProcessOptions struct {
	Program        string            `json:"program"`
	Args           []string          `json:"args,omitempty"`
	Directory      string            `json:"directory,omitempty"`
	Environment    map[string]string `json:"environment,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty"`
}

type ProcessResult struct {
	Program         string   `json:"program"`
	Args            []string `json:"args,omitempty"`
	Root            string   `json:"root"`
	Directory       string   `json:"directory"`
	ExitCode        int      `json:"exit_code"`
	Stdout          string   `json:"stdout"`
	Stderr          string   `json:"stderr"`
	StdoutTruncated bool     `json:"stdout_truncated"`
	StderrTruncated bool     `json:"stderr_truncated"`
	DurationMS      int64    `json:"duration_ms"`
	TimedOut        bool     `json:"timed_out"`
}
