package filesystem

import "time"

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
}

type TextReadResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

type TextWriteResult struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	Created bool   `json:"created"`
}

type TextEditResult struct {
	Path         string `json:"path"`
	Bytes        int    `json:"bytes"`
	Replacements int    `json:"replacements"`
}

type SearchMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}
