package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// An AI that explores a website can write down what it learned as a page tool, but only as a proposal: it waits in
// .proposed beside the catalog, exactly as the catalog will hold it, until the owner approves it from the extension.
// Approval moves it into the catalog and reloads; a catalog kept in Git gets one commit per approval.
const proposedDir = ".proposed"

// Proposes reports whether drafts have a catalog to wait beside.
func (r *Registry) Proposes() bool { return r != nil && r.root != "" }

// Proposal is one draft page tool and what approving it would do.
type Proposal struct {
	Definition Definition `json:"definition"`
	Script     string     `json:"script"`
	// Replaces is true when approving it would replace an approved tool of the same name.
	Replaces bool `json:"replaces"`
}

// Propose validates a draft exactly as the catalog would and stores it for the owner. A newer draft of the same name
// replaces the older one. Local app bindings are not proposed: they run programs, which is the owner's to write.
func (r *Registry) Propose(name string, definition json.RawMessage, script string) (Proposal, error) {
	if r == nil || r.root == "" {
		return Proposal{}, errors.New("capabilities are disabled: configure capabilities.directory")
	}
	parts := strings.Split(name, ".")
	if len(parts) < 2 || len(name) > 128 || !validName.MatchString(name) {
		return Proposal{}, errors.New("name a page tool namespace.action, such as chaoxing.course_outline")
	}
	var probe struct {
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal(definition, &probe) != nil {
		return Proposal{}, errors.New("definition must be a JSON object")
	}
	if probe.Command != nil {
		return Proposal{}, errors.New("only browser page tools can be proposed")
	}
	staging, err := os.MkdirTemp("", "lrmcp-proposal-")
	if err != nil {
		return Proposal{}, err
	}
	defer os.RemoveAll(staging)
	base := filepath.Join(staging, filepath.FromSlash(strings.Join(parts, "/")))
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return Proposal{}, err
	}
	if err := os.WriteFile(base+".json", definition, 0o644); err != nil {
		return Proposal{}, err
	}
	if err := os.WriteFile(base+".js", []byte(script), 0o644); err != nil {
		return Proposal{}, err
	}
	loaded, err := loadEntry(base+".json", name)
	if err != nil {
		return Proposal{}, fmt.Errorf("the draft does not validate: %w", err)
	}
	target := r.proposedPath(name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return Proposal{}, err
	}
	if err := os.WriteFile(target+".js", []byte(script), 0o644); err != nil {
		return Proposal{}, err
	}
	if err := os.WriteFile(target+".json", definition, 0o644); err != nil {
		return Proposal{}, err
	}
	return r.proposal(name, loaded.definition, script), nil
}

// Proposals lists the drafts waiting for the owner, by name.
func (r *Registry) Proposals() []Proposal {
	if r == nil || r.root == "" {
		return nil
	}
	root := filepath.Join(r.root, proposedDir)
	drafts := []Proposal{}
	_ = filepath.WalkDir(root, func(path string, item os.DirEntry, err error) error {
		if err != nil || item.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		relative, _ := filepath.Rel(root, path)
		name := strings.ReplaceAll(filepath.ToSlash(strings.TrimSuffix(relative, ".json")), "/", ".")
		loaded, err := loadEntry(path, name)
		if err != nil {
			return nil // A broken draft is not offered; a newer proposal replaces it.
		}
		script, _ := os.ReadFile(strings.TrimSuffix(path, ".json") + ".js")
		drafts = append(drafts, r.proposal(name, loaded.definition, string(script)))
		return nil
	})
	sort.Slice(drafts, func(i, j int) bool { return drafts[i].Definition.Name < drafts[j].Definition.Name })
	return drafts
}

// Approve moves a draft into the catalog and reloads it. If the catalog does not load with it, nothing changes.
func (r *Registry) Approve(name string) error {
	from, err := r.draft(name)
	if err != nil {
		return err
	}
	to := filepath.Join(r.root, filepath.FromSlash(strings.ReplaceAll(name, ".", "/")))
	kept := map[string][]byte{}
	for _, ext := range []string{".json", ".js"} {
		if old, err := os.ReadFile(to + ext); err == nil {
			kept[ext] = old
		}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	for _, ext := range []string{".js", ".json"} {
		data, err := os.ReadFile(from + ext)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to+ext, data, 0o644); err != nil {
			return err
		}
	}
	if err := r.Reload(); err != nil {
		for _, ext := range []string{".json", ".js"} {
			if old, ok := kept[ext]; ok {
				_ = os.WriteFile(to+ext, old, 0o644)
			} else {
				_ = os.Remove(to + ext)
			}
		}
		return fmt.Errorf("the catalog does not load with this tool: %w", err)
	}
	_ = os.Remove(from + ".json")
	_ = os.Remove(from + ".js")
	r.commit(name, to)
	return nil
}

// Reject discards a draft.
func (r *Registry) Reject(name string) error {
	from, err := r.draft(name)
	if err != nil {
		return err
	}
	_ = os.Remove(from + ".js")
	return os.Remove(from + ".json")
}

func (r *Registry) proposedPath(name string) string {
	return filepath.Join(r.root, proposedDir, filepath.FromSlash(strings.ReplaceAll(name, ".", "/")))
}

func (r *Registry) draft(name string) (string, error) {
	if r == nil || r.root == "" || !validName.MatchString(name) || !strings.Contains(name, ".") {
		return "", errors.New("no such proposal")
	}
	from := r.proposedPath(name)
	if _, err := os.Stat(from + ".json"); err != nil {
		return "", errors.New("no such proposal")
	}
	return from, nil
}

func (r *Registry) proposal(name string, d Definition, script string) Proposal {
	r.mu.RLock()
	_, replaces := r.entries[name]
	r.mu.RUnlock()
	return Proposal{Definition: d, Script: script, Replaces: replaces}
}

// commit records an approval when the catalog is a Git repository, so every version of a distilled tool can be
// read back and restored. A catalog outside Git is simply the files.
func (r *Registry) commit(name, path string) {
	if _, err := os.Stat(filepath.Join(r.root, ".git")); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	relative, _ := filepath.Rel(r.root, path)
	add := exec.CommandContext(ctx, "git", "-C", r.root, "add", "--", relative+".json", relative+".js")
	if add.Run() != nil {
		return
	}
	_ = exec.CommandContext(ctx, "git", "-C", r.root, "commit", "-q", "-m", "feat("+strings.SplitN(name, ".", 2)[0]+"): approve "+name, "--", relative+".json", relative+".js").Run()
}
