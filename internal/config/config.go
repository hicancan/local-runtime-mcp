package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ConfigEnvironment = "LRMCP_CONFIG"
	DefaultListen     = "127.0.0.1:9315"
)

// Config contains only the state needed by the optional browser bridge.
// Files, processes, images, and the desktop use the machine directly.
type Config struct {
	Browser Browser `yaml:"browser,omitempty"`
}

type Browser struct {
	Listen string `yaml:"listen,omitempty"`
	Token  string `yaml:"token,omitempty"`
}

func Default() *Config {
	return &Config{Browser: Browser{Listen: DefaultListen}}
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

func Path(path string) (string, error) {
	if path == "" {
		return DefaultPath()
	}
	return filepath.Abs(path)
}

// Load reads the optional browser configuration. A missing file is the valid,
// browser-unconfigured default rather than a setup error.
func Load(path string) (*Config, error) {
	resolved, err := Path(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	cfg := Default()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}
	if cfg.Browser.Listen == "" {
		cfg.Browser.Listen = DefaultListen
	}
	token, missing := expandEnvironment(cfg.Browser.Token)
	if len(missing) > 0 {
		return nil, fmt.Errorf("browser token references unset environment variable %s", strings.Join(missing, ", "))
	}
	cfg.Browser.Token = token
	if err := validateBrowser(cfg.Browser); err != nil {
		return nil, err
	}
	return cfg, nil
}

func Save(path string, cfg *Config) (string, error) {
	resolved, err := Path(path)
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "", errors.New("configuration cannot be nil")
	}
	if cfg.Browser.Listen == "" {
		cfg.Browser.Listen = DefaultListen
	}
	if err := validateBrowser(cfg.Browser); err != nil {
		return "", err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("encode configuration: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o700); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(filepath.Dir(resolved), ".lrmcp-config-*")
	if err != nil {
		return "", err
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := replaceFile(temporaryName, resolved); err != nil {
		return "", err
	}
	committed = true
	return resolved, nil
}

func validateBrowser(browser Browser) error {
	host, _, err := net.SplitHostPort(browser.Listen)
	if err != nil {
		return fmt.Errorf("browser listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return errors.New("browser listen address must use a loopback host")
	}
	if browser.Token != "" && len(browser.Token) < 32 {
		return errors.New("browser token must contain at least 32 characters")
	}
	return nil
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
