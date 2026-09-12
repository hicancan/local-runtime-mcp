package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the complete wmcp configuration.
type Config struct {
	Workspaces map[string]Workspace `yaml:"workspaces"`
}

// Workspace names one local project directory.
type Workspace struct {
	Root        string `yaml:"root"`
	Description string `yaml:"description,omitempty"`
}

// DefaultPath returns WMCP_CONFIG or %USERPROFILE%/.wmcp/config.yaml.
func DefaultPath() (string, error) {
	if path := os.Getenv("WMCP_CONFIG"); path != "" {
		return filepath.Abs(path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home: %w", err)
	}
	return filepath.Join(home, ".wmcp", "config.yaml"), nil
}

// Load reads and validates a configuration file. When no configuration is
// supplied and the default file does not exist, wmcp exposes the current
// working directory as a single workspace named "workspace".
func Load(path string) (*Config, error) {
	explicit := path != "" || os.Getenv("WMCP_CONFIG") != ""
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if !explicit {
				root, cwdErr := os.Getwd()
				if cwdErr != nil {
					return nil, fmt.Errorf("find current working directory: %w", cwdErr)
				}
				return &Config{Workspaces: map[string]Workspace{
					"workspace": {Root: filepath.Clean(root), Description: "Current working directory"},
				}}, nil
			}
			return nil, fmt.Errorf("configuration not found at %s; create it or set WMCP_CONFIG", path)
		}
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}
	if len(cfg.Workspaces) == 0 {
		return nil, errors.New("configuration contains no workspaces")
	}
	base := filepath.Dir(path)
	for name, ws := range cfg.Workspaces {
		if name == "" {
			return nil, errors.New("workspace name cannot be empty")
		}
		if ws.Root == "" {
			return nil, fmt.Errorf("workspace %q has no root", name)
		}
		root := os.ExpandEnv(ws.Root)
		if !filepath.IsAbs(root) {
			root = filepath.Join(base, root)
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("workspace %q root: %w", name, err)
		}
		ws.Root = filepath.Clean(abs)
		cfg.Workspaces[name] = ws
	}
	return &cfg, nil
}
