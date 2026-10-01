# Third-party notices

Local Runtime MCP is licensed under GNU AGPL v3.0 only. The following components retain their respective upstream licenses and attribution notices.

## OpenAI Tunnel Client

The `lrmcp` executable incorporates [OpenAI Tunnel Client](https://github.com/openai/tunnel-client), Go module `github.com/openai/tunnel-client` version `v0.0.14`, as a statically linked dependency for the OpenAI Tunnel adapter.

The upstream component is licensed under Apache License 2.0. Its license and attribution notice are reproduced in [licenses/OpenAI-Tunnel-Client-LICENSE](licenses/OpenAI-Tunnel-Client-LICENSE) and [licenses/OpenAI-Tunnel-Client-NOTICE](licenses/OpenAI-Tunnel-Client-NOTICE), included with source and release archives.

## Cloudflare Tunnel

Release archives include the unmodified `cloudflared` companion binary, version 2026.9.1, from the [Cloudflare Tunnel project](https://github.com/cloudflare/cloudflared).

`cloudflared` is a separate program distributed under the Apache License 2.0. Local Runtime MCP invokes it as a child process and does not link or incorporate its source code. Its license is included in release archives as `cloudflared-LICENSE`.
