package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const ConfigEnvironment = "LRMCP_CONFIG"

// Config is the complete Local Runtime MCP configuration.
type Config struct {
	Roots    map[string]Root `yaml:"roots"`
	Browser  Browser         `yaml:"browser,omitempty"`
	Computer Computer        `yaml:"computer,omitempty"`
}

// Root is a named filesystem boundary on the machine running the server.
type Root struct {
	Path        string `yaml:"path"`
	Description string `yaml:"description,omitempty"`
}

// Browser configures the loopback bridge used by the bundled Chromium extension.
type Browser struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen,omitempty"`
	Token   string `yaml:"token,omitempty"`
}

// Computer controls whether MCP clients can operate the desktop UI.
type Computer struct {
	Enabled bool `yaml:"enabled"`
}

// DefaultPath returns LRMCP_CONFIG or %USERPROFILE%/.lrmcp/config.yaml.
func DefaultPath() (string, error) {
	if path := os.Getenv(ConfigEnvironment); path != "" {
		return filepath.Abs(path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home: %w", err)
	}
	return filepath.Join(home, ".lrmcp", "config.yaml"), nil
}

// Load reads and validates configuration. If the default configuration does
// not exist, the current directory is exposed as the root named "default".
func Load(path string) (*Config, error) {
	explicit := path != "" || os.Getenv(ConfigEnvironment) != ""
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			cwd, cwdErr := os.Getwd()
			if cwdErr != nil {
				return nil, fmt.Errorf("find current working directory: %w", cwdErr)
			}
			root, rootErr := canonicalDirectory(cwd)
			if rootErr != nil {
				return nil, rootErr
			}
			return &Config{Roots: map[string]Root{
				"default": {Path: root, Description: "Current working directory"},
			}}, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("configuration not found at %s; create it or set %s", path, ConfigEnvironment)
		}
		return nil, fmt.Errorf("read configuration: %w", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}
	if len(cfg.Roots) == 0 {
		return nil, errors.New("configuration contains no roots")
	}

	base := filepath.Dir(path)
	names := make([]string, 0, len(cfg.Roots))
	for name := range cfg.Roots {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		root := cfg.Roots[name]
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("root name cannot be empty")
		}
		if strings.TrimSpace(root.Path) == "" {
			return nil, fmt.Errorf("root %q has no path", name)
		}
		expanded, missing := expandEnvironment(root.Path)
		if len(missing) > 0 {
			return nil, fmt.Errorf("root %q references unset environment variable %s", name, strings.Join(missing, ", "))
		}
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(base, expanded)
		}
		canonical, err := canonicalDirectory(expanded)
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", name, err)
		}
		root.Path = canonical
		cfg.Roots[name] = root
	}
	if cfg.Browser.Enabled {
		if cfg.Browser.Listen == "" {
			cfg.Browser.Listen = "127.0.0.1:9315"
		}
		if !strings.HasPrefix(cfg.Browser.Listen, "127.0.0.1:") && !strings.HasPrefix(cfg.Browser.Listen, "localhost:") && !strings.HasPrefix(cfg.Browser.Listen, "[::1]:") {
			return nil, errors.New("browser listen address must use a loopback host")
		}
		token, missing := expandEnvironment(cfg.Browser.Token)
		if len(missing) > 0 {
			return nil, fmt.Errorf("browser token references unset environment variable %s", strings.Join(missing, ", "))
		}
		if len(token) < 32 {
			return nil, errors.New("browser token must contain at least 32 characters")
		}
		cfg.Browser.Token = token
	}
	return &cfg, nil
}

func canonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", abs, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat path %s: %w", canonical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path %s is not a directory", canonical)
	}
	return filepath.Clean(canonical), nil
}

func expandEnvironment(value string) (string, []string) {
	missingSet := make(map[string]struct{})
	expanded := os.Expand(value, func(name string) string {
		if resolved, ok := os.LookupEnv(name); ok {
			return resolved
		}
		missingSet[name] = struct{}{}
		return ""
	})
	missing := make([]string, 0, len(missingSet))
	for name := range missingSet {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return expanded, missing
}
