package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hicancan/workspace-mcp/internal/config"
)

// Manager resolves configured workspace names and exposes their operations.
type Manager struct {
	workspaces map[string]config.Workspace
}

func NewManager(cfg *config.Config) *Manager {
	return &Manager{workspaces: cfg.Workspaces}
}

func (m *Manager) Root(name string) (string, error) {
	ws, ok := m.workspaces[name]
	if !ok {
		return "", fmt.Errorf("unknown workspace %q", name)
	}
	return ws.Root, nil
}

func (m *Manager) Path(name, path string) (string, error) {
	root, err := m.Root(name)
	if err != nil {
		return "", err
	}
	if path == "" || path == "." {
		return root, nil
	}
	return filepath.Clean(filepath.Join(root, filepath.FromSlash(path))), nil
}

func (m *Manager) List() []Info {
	names := make([]string, 0, len(m.workspaces))
	for name := range m.workspaces {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]Info, 0, len(names))
	for _, name := range names {
		result = append(result, m.Info(name))
	}
	return result
}

func (m *Manager) Info(name string) Info {
	ws, ok := m.workspaces[name]
	if !ok {
		return Info{Name: name, Capabilities: map[string]bool{}}
	}
	_, err := os.Stat(ws.Root)
	return Info{
		Name:         name,
		Description:  ws.Description,
		Exists:       err == nil,
		Capabilities: detectCapabilities(ws.Root),
	}
}

func detectCapabilities(root string) map[string]bool {
	has := func(paths ...string) bool {
		for _, path := range paths {
			if _, err := os.Stat(filepath.Join(root, path)); err == nil {
				return true
			}
		}
		return false
	}
	latex := false
	if entries, err := filepath.Glob(filepath.Join(root, "*.tex")); err == nil && len(entries) > 0 {
		latex = true
	}
	if !latex {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || latex {
				return nil
			}
			if d.IsDir() && strings.EqualFold(d.Name(), ".git") {
				return filepath.SkipDir
			}
			if strings.EqualFold(filepath.Ext(d.Name()), ".tex") {
				latex = true
			}
			return nil
		})
	}
	gitPath, _ := exec.LookPath("git")
	xelatexPath, _ := exec.LookPath("xelatex")
	pythonPath, _ := exec.LookPath("python")
	nodePath, _ := exec.LookPath("node")
	goPath, _ := exec.LookPath("go")
	return map[string]bool{
		"files":  true,
		"git":    gitPath != "" && has(".git"),
		"latex":  latex && xelatexPath != "",
		"python": pythonPath != "" && has("pyproject.toml", "requirements.txt", "setup.py"),
		"node":   nodePath != "" && has("package.json"),
		"go":     goPath != "" && has("go.mod"),
	}
}
