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

type TextPatchResult struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	Hunks  int    `json:"hunks"`
	SHA256 string `json:"sha256"`
}

// TextPatchHunk identifies one exact edit against the original file. Before
// and After are context, not replacement text. Old may be empty for an
// insertion, but then at least one context field is required.
type TextPatchHunk struct {
	Before string `json:"before,omitempty"`
	Old    string `json:"old"`
	New    string `json:"new"`
	After  string `json:"after,omitempty"`
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
	CreateParents  bool
}

type PatchTextOptions struct {
	Hunks          []TextPatchHunk
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
