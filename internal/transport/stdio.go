package transport

import (
	"context"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func ServeStdio(ctx context.Context, server *mcp.Server, stdin io.Reader, stdout io.Writer) error {
	return server.Run(ctx, &mcp.IOTransport{
		Reader: noCloseReader{stdin},
		Writer: noCloseWriter{stdout},
	})
}

type noCloseReader struct{ io.Reader }

func (noCloseReader) Close() error { return nil }

type noCloseWriter struct{ io.Writer }

func (noCloseWriter) Close() error { return nil }
