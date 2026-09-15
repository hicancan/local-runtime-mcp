package filesystem

import "time"

type FileEntry struct {
	Path    string    `json:"path"`
	Type    string    `json:"type"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"modified_at"`
}

type ListResult struct {
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
	Skipped   int         `json:"skipped"`
}

type FileInfo struct {
	Path          string    `json:"path"`
	Type          string    `json:"type"`
	Size          int64     `json:"size"`
	ModTime       time.Time `json:"modified_at"`
	MIMEType      string    `json:"mime_type,omitempty"`
	SymlinkTarget string    `json:"symlink_target,omitempty"`
	SHA256        string    `json:"sha256,omitempty"`
}

type TextReadResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	NextLine  int    `json:"next_line,omitempty"`
	Truncated bool   `json:"truncated"`
	SHA256    string `json:"sha256,omitempty"`
}

type TextWriteResult struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	Created bool   `json:"created"`
	SHA256  string `json:"sha256"`
}

type TextEditResult struct {
	Path         string `json:"path"`
	Bytes        int    `json:"bytes"`
	Replacements []int  `json:"replacements"`
	SHA256       string `json:"sha256"`
}

type TextEdit struct {
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type ReadTextOptions struct {
	StartLine  int
	LineCount  int
	MaxBytes   int
	WithSHA256 bool
}

type WriteTextOptions struct {
	CreateOnly     bool
	ExpectedSHA256 string
}

type EditTextOptions struct {
	Edits          []TextEdit
	ExpectedSHA256 string
}

type SearchMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

type SearchResult struct {
	Matches   []SearchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
	Skipped   int           `json:"skipped"`
}
