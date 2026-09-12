package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hicancan/workspace-mcp/internal/config"
	"github.com/hicancan/workspace-mcp/internal/mcpserver"
	"github.com/hicancan/workspace-mcp/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	tunnelclient "github.com/openai/tunnel-client"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "wmcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "tunnel":
		return tunnel(args[1:])
	case "workspace":
		return workspaceCommand(args[1:])
	case "version", "--version", "-v":
		fmt.Println("wmcp", mcpserver.Version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "configuration file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manager, err := loadManager(*configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = mcpserver.New(manager).Run(ctx, &mcp.StdioTransport{})
	if err == nil || errors.Is(err, context.Canceled) || strings.HasPrefix(err.Error(), "server is closing:") {
		return nil
	}
	return err
}

func tunnel(args []string) error {
	flags := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	configPath := flags.String("config", "", "configuration file path")
	tunnelID := flags.String("tunnel-id", os.Getenv("CONTROL_PLANE_TUNNEL_ID"), "OpenAI tunnel ID (or CONTROL_PLANE_TUNNEL_ID)")
	apiKeyEnv := flags.String("api-key-env", "CONTROL_PLANE_API_KEY", "environment variable containing the tunnel runtime API key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *tunnelID == "" {
		return errors.New("tunnel ID is required via --tunnel-id or CONTROL_PLANE_TUNNEL_ID")
	}
	apiKey := os.Getenv(*apiKeyEnv)
	if apiKey == "" {
		return fmt.Errorf("tunnel API key environment variable %s is empty", *apiKeyEnv)
	}
	manager, err := loadManager(*configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverTransport, tunnelTransport := mcp.NewInMemoryTransports()
	server := mcpserver.New(manager)
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Run(ctx, serverTransport) }()
	client, err := tunnelclient.New(tunnelclient.Config{TunnelID: *tunnelID, APIKey: apiKey}, tunnelTransport)
	if err != nil {
		return fmt.Errorf("create tunnel client: %w", err)
	}
	tunnelErr := make(chan error, 1)
	go func() { tunnelErr <- client.Run(ctx) }()
	select {
	case err := <-serverErr:
		return err
	case err := <-tunnelErr:
		return err
	case <-ctx.Done():
		return nil
	}
}

func workspaceCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("workspace requires 'list' or 'info NAME'")
	}
	flags := flag.NewFlagSet("workspace", flag.ContinueOnError)
	configPath := flags.String("config", "", "configuration file path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	manager, err := loadManager(*configPath)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	switch args[0] {
	case "list":
		return encoder.Encode(manager.List())
	case "info":
		rest := flags.Args()
		if len(rest) != 1 {
			return errors.New("usage: wmcp workspace info [--config PATH] NAME")
		}
		if _, err := manager.Root(rest[0]); err != nil {
			return err
		}
		return encoder.Encode(manager.Info(rest[0]))
	default:
		return fmt.Errorf("unknown workspace command %q", args[0])
	}
}

func loadManager(path string) (*workspace.Manager, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return workspace.NewManager(cfg), nil
}

func usage() {
	fmt.Print(`Workspace MCP (wmcp)

Usage:
  wmcp serve [--config PATH]
  wmcp tunnel [--config PATH] [--tunnel-id ID] [--api-key-env NAME]
  wmcp workspace list [--config PATH]
  wmcp workspace info [--config PATH] NAME
  wmcp version
`)
}
