package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/config"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	tunnelclient "github.com/openai/tunnel-client"
)

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return stdio(ctx, args, stdin, stdout, stderr)
	}
	switch args[0] {
	case "tunnel":
		return tunnel(ctx, args[1:], stderr)
	case "browser-setup":
		return browserSetup(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		_, err := fmt.Fprintln(stdout, "lrmcp", mcpserver.Version)
		return err
	case "help", "--help", "-h":
		usage(stdout)
		return nil
	}
	if strings.HasPrefix(args[0], "-") {
		return stdio(ctx, args, stdin, stdout, stderr)
	}
	return fmt.Errorf("unknown command %q; run lrmcp help", args[0])
}

func stdio(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flagSet("lrmcp", stderr)
	configPath := flags.String("config", "", "configuration file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	bridge, err := browser.Start(ctx, cfg.Browser)
	if err != nil {
		return err
	}
	defer bridge.Close(context.Background())
	err = mcpserver.New(ctx, bridge, computer.New()).Run(ctx, &mcp.IOTransport{Reader: noCloseReader{stdin}, Writer: noCloseWriter{stdout}})
	return normalizeCancellation(err)
}

func tunnel(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flagSet("tunnel", stderr)
	configPath := flags.String("config", "", "configuration file path")
	tunnelID := flags.String("tunnel-id", os.Getenv("CONTROL_PLANE_TUNNEL_ID"), "OpenAI tunnel ID")
	apiKeyEnv := flags.String("api-key-env", "CONTROL_PLANE_API_KEY", "environment variable containing the tunnel API key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *tunnelID == "" {
		return errors.New("tunnel ID is required via --tunnel-id or CONTROL_PLANE_TUNNEL_ID")
	}
	apiKey := os.Getenv(*apiKeyEnv)
	if apiKey == "" {
		return fmt.Errorf("tunnel API key environment variable %s is empty", *apiKeyEnv)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	bridge, err := browser.Start(ctx, cfg.Browser)
	if err != nil {
		return err
	}
	defer bridge.Close(context.Background())
	serverTransport, tunnelTransport := mcp.NewInMemoryTransports()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- mcpserver.New(ctx, bridge, computer.New()).Run(ctx, serverTransport) }()
	client, err := tunnelclient.New(tunnelclient.Config{TunnelID: *tunnelID, APIKey: apiKey}, tunnelTransport)
	if err != nil {
		return fmt.Errorf("create tunnel client: %w", err)
	}
	tunnelErrors := make(chan error, 1)
	go func() { tunnelErrors <- client.Run(ctx) }()
	select {
	case err := <-serverErrors:
		return normalizeCancellation(err)
	case err := <-tunnelErrors:
		return normalizeCancellation(err)
	case <-ctx.Done():
		return nil
	}
}

func browserSetup(args []string, stdout, stderr io.Writer) error {
	flags := flagSet("browser-setup", stderr)
	configPath := flags.String("config", "", "configuration file path")
	defaultDirectory, err := browser.DefaultExtensionDirectory()
	if err != nil {
		return err
	}
	directory := flags.String("directory", defaultDirectory, "extension output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Browser.Token == "" {
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return err
		}
		cfg.Browser.Token = hex.EncodeToString(value)
	}
	resolvedConfig, err := config.Save(*configPath, cfg)
	if err != nil {
		return err
	}
	installed, err := browser.InstallExtension(*directory)
	if err != nil {
		return err
	}
	if err := browser.ConfigureExtension(installed, cfg.Browser.Listen, cfg.Browser.Token); err != nil {
		return err
	}
	return writeJSON(stdout, struct {
		Directory string `json:"directory"`
		Config    string `json:"config"`
		Address   string `json:"address"`
		NextStep  string `json:"next_step"`
	}{
		Directory: installed,
		Config:    resolvedConfig,
		Address:   cfg.Browser.Listen,
		NextStep:  "Open edge://extensions or chrome://extensions, enable Developer mode, choose Load unpacked, and select the extension directory.",
	})
}

func flagSet(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func normalizeCancellation(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || strings.HasPrefix(err.Error(), "server is closing:") {
		return nil
	}
	return err
}

func usage(writer io.Writer) {
	fmt.Fprint(writer, `Local Runtime MCP (lrmcp)

Usage:
  lrmcp [--config PATH]                  Serve MCP over stdio
  lrmcp tunnel [flags]                   Connect this machine to an OpenAI tunnel
  lrmcp browser-setup [flags]            Configure and extract the Chromium extension
  lrmcp version
  lrmcp help

Tunnel flags:
  --config PATH
  --tunnel-id ID                         Or CONTROL_PLANE_TUNNEL_ID
  --api-key-env NAME                     Defaults to CONTROL_PLANE_API_KEY
`)
}

type noCloseReader struct{ io.Reader }

func (noCloseReader) Close() error { return nil }

type noCloseWriter struct{ io.Writer }

func (noCloseWriter) Close() error { return nil }
