package cli

import (
	"context"
	"encoding/base64"
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
	"github.com/hicancan/local-runtime-mcp/internal/core"
	"github.com/hicancan/local-runtime-mcp/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	tunnelclient "github.com/openai/tunnel-client"
)

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stdout)
		return nil
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stdin, stdout, stderr)
	case "tunnel":
		return tunnel(ctx, args[1:], stderr)
	case "filesystem":
		return filesystem(ctx, args[1:], stdin, stdout, stderr)
	case "image":
		return imageCommand(args[1:], stdout, stderr)
	case "process":
		return processCommand(ctx, args[1:], stdin, stdout, stderr)
	case "browser":
		return browserCommand(ctx, args[1:], stdout, stderr)
	case "computer":
		return computerCommand(ctx, args[1:], stdout, stderr)
	case "version", "--version", "-v":
		_, err := fmt.Fprintln(stdout, "lrmcp", mcpserver.Version)
		return err
	case "help", "--help", "-h":
		usage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flagSet("serve", stderr)
	configPath := flags.String("config", "", "configuration file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, runtime, err := loadConfigRuntime(*configPath)
	if err != nil {
		return err
	}
	bridge, err := browser.Start(ctx, cfg.Browser)
	if err != nil {
		return err
	}
	defer bridge.Close(context.Background())
	var controller computer.Controller
	if cfg.Computer.Enabled {
		controller = computer.New()
	}
	err = mcpserver.New(runtime, bridge, controller).Run(ctx, &mcp.IOTransport{Reader: noCloseReader{stdin}, Writer: noCloseWriter{stdout}})
	if err == nil || errors.Is(err, context.Canceled) || strings.HasPrefix(err.Error(), "server is closing:") {
		return nil
	}
	return err
}

func tunnel(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flagSet("tunnel", stderr)
	configPath := flags.String("config", "", "configuration file path")
	tunnelID := flags.String("tunnel-id", os.Getenv("CONTROL_PLANE_TUNNEL_ID"), "OpenAI tunnel ID")
	apiKeyEnv := flags.String("api-key-env", "CONTROL_PLANE_API_KEY", "environment variable containing the tunnel API key")
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
	cfg, runtime, err := loadConfigRuntime(*configPath)
	if err != nil {
		return err
	}
	bridge, err := browser.Start(ctx, cfg.Browser)
	if err != nil {
		return err
	}
	defer bridge.Close(context.Background())
	var controller computer.Controller
	if cfg.Computer.Enabled {
		controller = computer.New()
	}
	serverTransport, tunnelTransport := mcp.NewInMemoryTransports()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- mcpserver.New(runtime, bridge, controller).Run(ctx, serverTransport) }()
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

func filesystem(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("filesystem requires roots, list, stat, read, write, edit, or search")
	}
	switch args[0] {
	case "roots":
		flags := flagSet("filesystem roots", stderr)
		configPath := flags.String("config", "", "configuration file path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		runtime, err := loadRuntime(*configPath)
		if err != nil {
			return err
		}
		return writeJSON(stdout, runtime.Roots())
	case "list":
		flags := flagSet("filesystem list", stderr)
		configPath, root := commonRootFlags(flags)
		path := flags.String("path", "", "root-relative directory path")
		maxDepth := flags.Int("max-depth", 4, "maximum traversal depth")
		includeHidden := flags.Bool("include-hidden", false, "include dotfiles")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		runtime, err := loadRuntime(*configPath)
		if err != nil {
			return err
		}
		result, err := runtime.List(*root, *path, *maxDepth, *includeHidden)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "stat":
		flags := flagSet("filesystem stat", stderr)
		configPath, root := commonRootFlags(flags)
		path := flags.String("path", "", "root-relative path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		runtime, err := loadRuntime(*configPath)
		if err != nil {
			return err
		}
		result, err := runtime.Stat(*root, *path)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "read":
		flags := flagSet("filesystem read", stderr)
		configPath, root := commonRootFlags(flags)
		path := flags.String("path", "", "root-relative text file path")
		maxBytes := flags.Int("max-bytes", 0, "maximum returned bytes")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		runtime, err := loadRuntime(*configPath)
		if err != nil {
			return err
		}
		result, err := runtime.ReadText(*root, *path, *maxBytes)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "write":
		flags := flagSet("filesystem write", stderr)
		configPath, root := commonRootFlags(flags)
		path := flags.String("path", "", "root-relative text file path")
		content := flags.String("content", "", "complete UTF-8 content")
		fromStdin := flags.Bool("stdin", false, "read complete content from stdin")
		expectedSHA := flags.String("expected-sha256", "", "required previous SHA-256")
		createOnly := flags.Bool("create-only", false, "fail if target exists")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		value, err := inputValue(stdin, *content, *fromStdin)
		if err != nil {
			return err
		}
		runtime, err := loadRuntime(*configPath)
		if err != nil {
			return err
		}
		result, err := runtime.WriteText(*root, *path, value, *expectedSHA, *createOnly)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "edit":
		flags := flagSet("filesystem edit", stderr)
		configPath, root := commonRootFlags(flags)
		path := flags.String("path", "", "root-relative text file path")
		oldText := flags.String("old", "", "exact text to replace")
		newText := flags.String("new", "", "replacement text")
		expectedSHA := flags.String("expected-sha256", "", "required current SHA-256")
		replaceAll := flags.Bool("replace-all", false, "replace every match")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		runtime, err := loadRuntime(*configPath)
		if err != nil {
			return err
		}
		result, err := runtime.EditText(*root, *path, *oldText, *newText, *expectedSHA, *replaceAll)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "search":
		flags := flagSet("filesystem search", stderr)
		configPath, root := commonRootFlags(flags)
		path := flags.String("path", "", "root-relative search path")
		query := flags.String("query", "", "text or regular expression")
		regex := flags.Bool("regex", false, "interpret query as a Go regular expression")
		caseSensitive := flags.Bool("case-sensitive", false, "match case")
		includeHidden := flags.Bool("include-hidden", false, "include dotfiles")
		maxResults := flags.Int("max-results", 0, "maximum result count")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		runtime, err := loadRuntime(*configPath)
		if err != nil {
			return err
		}
		result, err := runtime.SearchText(*root, *path, *query, *regex, *caseSensitive, *includeHidden, *maxResults)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	default:
		return fmt.Errorf("unknown filesystem command %q", args[0])
	}
}

func imageCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "read" {
		return errors.New("image requires 'read'")
	}
	flags := flagSet("image read", stderr)
	configPath, root := commonRootFlags(flags)
	path := flags.String("path", "", "root-relative image path")
	maxBytes := flags.Int("max-bytes", 0, "maximum complete image size")
	metadataOnly := flags.Bool("metadata-only", false, "omit base64 image data")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	runtime, err := loadRuntime(*configPath)
	if err != nil {
		return err
	}
	data, metadata, err := runtime.ReadImage(*root, *path, *maxBytes)
	if err != nil {
		return err
	}
	result := struct {
		Metadata core.ImageReadResult `json:"metadata"`
		Data     string               `json:"data_base64,omitempty"`
	}{Metadata: metadata}
	if !*metadataOnly {
		result.Data = base64.StdEncoding.EncodeToString(data)
	}
	return writeJSON(stdout, result)
}

func processCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "run" {
		return errors.New("process requires 'run'")
	}
	flags := flagSet("process run", stderr)
	configPath, root := commonRootFlags(flags)
	directory := flags.String("directory", "", "working directory relative to the root")
	stdinText := flags.String("stdin-text", "", "text sent to stdin")
	fromStdin := flags.Bool("stdin", false, "read process stdin from this CLI's stdin")
	timeout := flags.Int("timeout", 0, "timeout in seconds")
	maxOutput := flags.Int("max-output-bytes", 0, "separate stdout and stderr limit")
	var environment environmentFlags
	flags.Var(&environment, "env", "environment variable NAME=VALUE; repeatable")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	programArgs := flags.Args()
	if len(programArgs) == 0 {
		return errors.New("program is required after flags (use -- before program flags)")
	}
	input, err := inputValue(stdin, *stdinText, *fromStdin)
	if err != nil {
		return err
	}
	runtime, err := loadRuntime(*configPath)
	if err != nil {
		return err
	}
	result, err := runtime.RunProcess(ctx, *root, core.ProcessOptions{
		Program: programArgs[0], Args: programArgs[1:], Directory: *directory,
		Environment: environment.values, Stdin: input, TimeoutSeconds: *timeout, MaxOutputBytes: *maxOutput,
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

type environmentFlags struct{ values map[string]string }

func (e *environmentFlags) String() string { return "" }

func (e *environmentFlags) Set(value string) error {
	name, content, ok := strings.Cut(value, "=")
	if !ok || name == "" {
		return errors.New("environment value must use NAME=VALUE")
	}
	if e.values == nil {
		e.values = make(map[string]string)
	}
	e.values[name] = content
	return nil
}

func commonRootFlags(flags *flag.FlagSet) (*string, *string) {
	return flags.String("config", "", "configuration file path"), flags.String("root", "default", "configured root name")
}

func flagSet(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

func inputValue(reader io.Reader, direct string, fromStdin bool) (string, error) {
	if !fromStdin {
		return direct, nil
	}
	if direct != "" {
		return "", errors.New("direct input and --stdin are mutually exclusive")
	}
	data, err := io.ReadAll(reader)
	return string(data), err
}

func loadRuntime(path string) (*core.Runtime, error) {
	_, runtime, err := loadConfigRuntime(path)
	return runtime, err
}

func loadConfigRuntime(path string) (*config.Config, *core.Runtime, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	return cfg, core.New(*cfg), nil
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
  lrmcp serve [--config PATH]
  lrmcp tunnel [--config PATH] [--tunnel-id ID] [--api-key-env NAME]
  lrmcp filesystem roots [--config PATH]
  lrmcp filesystem list|stat|read|write|edit|search [flags]
  lrmcp image read [flags]
  lrmcp process run [flags] -- PROGRAM [ARG...]
  lrmcp browser extension|token|status|tabs|open|close|navigate|snapshot|screenshot|action [flags]
  lrmcp computer screenshot|action [flags]
  lrmcp version
`)
}

type noCloseReader struct{ io.Reader }

func (noCloseReader) Close() error { return nil }

type noCloseWriter struct{ io.Writer }

func (noCloseWriter) Close() error { return nil }
