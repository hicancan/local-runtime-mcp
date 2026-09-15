package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hicancan/local-runtime-mcp/internal/config"
)

type Runtime struct {
	roots map[string]RootInfo
}

func New(cfg config.Config) *Runtime {
	roots := make(map[string]RootInfo, len(cfg.Roots))
	for name, root := range cfg.Roots {
		// Config.Load already canonicalizes roots. Keep New safe for callers that
		// construct Config directly (notably embedders and tests), too. On macOS,
		// temporary directories are commonly exposed through /var while resolving
		// to /private/var; comparing one spelling with the other would otherwise
		// reject paths that are actually inside the configured root.
		path := root.Path
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if canonical, err := filepath.EvalSymlinks(path); err == nil {
			path = canonical
		}
		roots[name] = RootInfo{Name: name, Path: filepath.Clean(path), Description: root.Description}
	}
	return &Runtime{roots: roots}
}

func (r *Runtime) Roots() []RootInfo {
	result := make([]RootInfo, 0, len(r.roots))
	for _, root := range r.roots {
		result = append(result, root)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (r *Runtime) Root(name string) (RootInfo, error) {
	root, ok := r.roots[name]
	if !ok {
		return RootInfo{}, fmt.Errorf("unknown root %q", name)
	}
	return root, nil
}

func (r *Runtime) Resolve(rootName, relativePath string) (string, error) {
	root, err := r.Root(rootName)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(relativePath) || filepath.VolumeName(relativePath) != "" {
		return "", errors.New("path must be relative to the configured root")
	}
	clean := filepath.Clean(relativePath)
	if clean == "." {
		clean = ""
	}
	candidate := filepath.Join(root.Path, clean)
	if !within(root.Path, candidate) {
		return "", errors.New("path escapes the configured root")
	}

	ancestor := candidate
	var missing []string
	for {
		_, statErr := os.Lstat(ancestor)
		if statErr == nil {
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("cannot find an existing path ancestor")
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = parent
	}

	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", fmt.Errorf("resolve path links: %w", err)
	}
	if !within(root.Path, resolved) {
		return "", errors.New("path resolves outside the configured root")
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
	}
	if !within(root.Path, resolved) {
		return "", errors.New("path escapes the configured root")
	}
	return resolved, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func relativeSlash(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}
