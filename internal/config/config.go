package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	FileName          = "local-runtime-mcp.yaml"
	DefaultBrowser    = "127.0.0.1:9315"
	DefaultHTTP       = "127.0.0.1:9316"
	EnvironmentPrefix = "LOCAL_RUNTIME_MCP_"
)

const (
	EnvBrowserListen         = EnvironmentPrefix + "BROWSER_LISTEN"
	EnvBrowserToken          = EnvironmentPrefix + "BROWSER_TOKEN"
	EnvOpenAITunnelID        = EnvironmentPrefix + "OPENAI_TUNNEL_ID"
	EnvOpenAIAPIKey          = EnvironmentPrefix + "OPENAI_API_KEY"
	EnvHTTPListen            = EnvironmentPrefix + "HTTP_LISTEN"
	EnvHTTPPublicHost        = EnvironmentPrefix + "HTTP_PUBLIC_HOST"
	EnvHTTPBearerToken       = EnvironmentPrefix + "HTTP_BEARER_TOKEN"
	EnvCloudflareTunnelToken = EnvironmentPrefix + "CLOUDFLARE_TUNNEL_TOKEN"
	EnvCloudflared           = EnvironmentPrefix + "CLOUDFLARED"
)

// Config is the complete persistent configuration for one portable runtime.
// It lives beside the executable as local-runtime-mcp.yaml.
type Config struct {
	Browser    Browser    `yaml:"browser,omitempty"`
	OpenAI     OpenAI     `yaml:"openai,omitempty"`
	HTTP       HTTP       `yaml:"http,omitempty"`
	Cloudflare Cloudflare `yaml:"cloudflare,omitempty"`
}

type Browser struct {
	Listen string `yaml:"listen,omitempty"`
	Token  string `yaml:"token,omitempty"`
}

type OpenAI struct {
	TunnelID string `yaml:"tunnel_id,omitempty"`
	APIKey   string `yaml:"api_key,omitempty"`
}

type HTTP struct {
	Listen      string `yaml:"listen,omitempty"`
	PublicHost  string `yaml:"public_host,omitempty"`
	BearerToken string `yaml:"bearer_token,omitempty"`
}

type Cloudflare struct {
	TunnelToken string `yaml:"tunnel_token,omitempty"`
	Binary      string `yaml:"binary,omitempty"`
}

// Overrides contains values explicitly supplied on the command line. Empty
// values mean that the corresponding flag was not supplied.
type Overrides struct {
	BrowserListen         string
	BrowserToken          string
	OpenAITunnelID        string
	OpenAIAPIKey          string
	HTTPListen            string
	HTTPPublicHost        string
	HTTPBearerToken       string
	CloudflareTunnelToken string
	Cloudflared           string
}

func Default() *Config {
	return &Config{
		Browser: Browser{Listen: DefaultBrowser},
		HTTP:    HTTP{Listen: DefaultHTTP},
	}
}

// PathForExecutable returns the one configuration path owned by an
// executable. Passing an empty path resolves the current executable.
func PathForExecutable(executable string) (string, error) {
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("find executable: %w", err)
		}
	}
	resolved, err := filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	return filepath.Join(filepath.Dir(resolved), FileName), nil
}

// Load reads a fixed configuration path. A missing file yields built-in
// defaults, which keeps stdio usable before optional connections are set up.
func Load(path string) (*Config, error) {
	cfg, err := read(path)
	if err != nil {
		return nil, err
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func read(path string) (*Config, error) {
	b, err := os.ReadFile(path)
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
	applyDefaults(cfg)
	return cfg, nil
}

// Resolve applies the single precedence rule used by every entry point:
// command line overrides environment, environment overrides YAML, and YAML
// overrides built-in defaults.
func Resolve(path string, overrides Overrides) (*Config, error) {
	cfg, err := read(path)
	if err != nil {
		return nil, err
	}
	applyEnvironment(cfg, os.LookupEnv)
	applyOverrides(cfg, overrides)
	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func Save(path string, cfg *Config) (string, error) {
	if cfg == nil {
		return "", errors.New("configuration cannot be nil")
	}
	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return "", err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("encode configuration: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".local-runtime-mcp-*")
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
	if err := replaceFile(temporaryName, path); err != nil {
		return "", err
	}
	committed = true
	return path, nil
}

func applyEnvironment(cfg *Config, lookup func(string) (string, bool)) {
	assign := func(name string, target *string) {
		if value, ok := lookup(name); ok && value != "" {
			*target = value
		}
	}
	assign(EnvBrowserListen, &cfg.Browser.Listen)
	assign(EnvBrowserToken, &cfg.Browser.Token)
	assign(EnvOpenAITunnelID, &cfg.OpenAI.TunnelID)
	assign(EnvOpenAIAPIKey, &cfg.OpenAI.APIKey)
	assign(EnvHTTPListen, &cfg.HTTP.Listen)
	assign(EnvHTTPPublicHost, &cfg.HTTP.PublicHost)
	assign(EnvHTTPBearerToken, &cfg.HTTP.BearerToken)
	assign(EnvCloudflareTunnelToken, &cfg.Cloudflare.TunnelToken)
	assign(EnvCloudflared, &cfg.Cloudflare.Binary)
}

func applyOverrides(cfg *Config, overrides Overrides) {
	assign := func(value string, target *string) {
		if value != "" {
			*target = value
		}
	}
	assign(overrides.BrowserListen, &cfg.Browser.Listen)
	assign(overrides.BrowserToken, &cfg.Browser.Token)
	assign(overrides.OpenAITunnelID, &cfg.OpenAI.TunnelID)
	assign(overrides.OpenAIAPIKey, &cfg.OpenAI.APIKey)
	assign(overrides.HTTPListen, &cfg.HTTP.Listen)
	assign(overrides.HTTPPublicHost, &cfg.HTTP.PublicHost)
	assign(overrides.HTTPBearerToken, &cfg.HTTP.BearerToken)
	assign(overrides.CloudflareTunnelToken, &cfg.Cloudflare.TunnelToken)
	assign(overrides.Cloudflared, &cfg.Cloudflare.Binary)
}

func applyDefaults(cfg *Config) {
	if cfg.Browser.Listen == "" {
		cfg.Browser.Listen = DefaultBrowser
	}
	if cfg.HTTP.Listen == "" {
		cfg.HTTP.Listen = DefaultHTTP
	}
}

func validate(cfg *Config) error {
	if err := validateLoopback("browser", cfg.Browser.Listen); err != nil {
		return err
	}
	if err := validateLoopback("HTTP", cfg.HTTP.Listen); err != nil {
		return err
	}
	if cfg.Browser.Token != "" && len(cfg.Browser.Token) < 32 {
		return errors.New("browser token must contain at least 32 characters")
	}
	if cfg.HTTP.BearerToken != "" && len(cfg.HTTP.BearerToken) < 32 {
		return errors.New("HTTP bearer token must contain at least 32 characters")
	}
	return nil
}

func validateLoopback(name, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s listen address: %w", name, err)
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s listen address must use a loopback host", name)
	}
	return nil
}
