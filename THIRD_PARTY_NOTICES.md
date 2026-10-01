# Third-party notices

Local Runtime MCP is licensed under GNU AGPL v3.0 only. The following components retain their respective upstream licenses and attribution notices.

## OpenAI Tunnel Client

The `lrmcp` executable incorporates [OpenAI Tunnel Client](https://github.com/openai/tunnel-client), Go module `github.com/openai/tunnel-client` version `v0.0.14`, as a statically linked dependency for the OpenAI Tunnel adapter.

The upstream component is licensed under Apache License 2.0. Its license and attribution notice are reproduced in [licenses/OpenAI-Tunnel-Client-LICENSE](licenses/OpenAI-Tunnel-Client-LICENSE) and [licenses/OpenAI-Tunnel-Client-NOTICE](licenses/OpenAI-Tunnel-Client-NOTICE), included with source and release archives.

## Go runtime dependencies

The `lrmcp` executable statically links the Go modules listed below. The inventory is the union of `go list -deps ./cmd/lrmcp` for Windows amd64, Linux amd64/arm64, and macOS amd64/arm64 with `CGO_ENABLED=0`. It includes the licenses and required notices supplied with those modules, plus the BSD license of the Go standard library and runtime. The release workflow includes the entire `licenses/` directory.

The complete upstream texts retain their copyright notices, with line endings normalized for the repository. Additional copyright declarations from selected production Go and assembly files are retained in [Go-runtime-COPYRIGHTS](licenses/Go-runtime-COPYRIGHTS). Mixed-license components retain all applicable texts: the MCP SDK includes Apache-2.0 and retained MIT contributions; YAML includes MIT libyaml code; OpenTelemetry and Segmentio encoding include BSD Go-derived code; and Prometheus's vendored `gddo` code has its own BSD license. The MCP SDK's license also describes its documentation terms; this distribution incorporates SDK code.

| Component | Version | Applicable license | Included license and notice files |
| --- | --- | --- | --- |
| Go standard library and runtime | `go1.27.0` | BSD-3-Clause | [Go-stdlib-LICENSE](licenses/Go-stdlib-LICENSE) |
| `github.com/aymanbagabas/go-pty` | `v0.2.3` | MIT | [Go-github-com-aymanbagabas-go-pty-LICENSE](licenses/Go-github-com-aymanbagabas-go-pty-LICENSE) |
| `github.com/bahlo/generic-list-go` | `v0.2.0` | BSD-3-Clause | [Go-github-com-bahlo-generic-list-go-LICENSE](licenses/Go-github-com-bahlo-generic-list-go-LICENSE) |
| `github.com/beorn7/perks` | `v1.0.1` | MIT | [Go-github-com-beorn7-perks-LICENSE](licenses/Go-github-com-beorn7-perks-LICENSE) |
| `github.com/buger/jsonparser` | `v1.1.2` | MIT | [Go-github-com-buger-jsonparser-LICENSE](licenses/Go-github-com-buger-jsonparser-LICENSE) |
| `github.com/cespare/xxhash/v2` | `v2.3.0` | MIT | [Go-github-com-cespare-xxhash-v2-LICENSE.txt](licenses/Go-github-com-cespare-xxhash-v2-LICENSE.txt) |
| `github.com/creack/pty` | `v1.1.24` | MIT | [Go-github-com-creack-pty-LICENSE](licenses/Go-github-com-creack-pty-LICENSE) |
| `github.com/felixge/httpsnoop` | `v1.0.4` | MIT | [Go-github-com-felixge-httpsnoop-LICENSE.txt](licenses/Go-github-com-felixge-httpsnoop-LICENSE.txt) |
| `github.com/go-logr/logr` | `v1.4.3` | Apache-2.0 | [Go-github-com-go-logr-logr-LICENSE](licenses/Go-github-com-go-logr-logr-LICENSE) |
| `github.com/go-logr/stdr` | `v1.2.2` | Apache-2.0 | [Go-github-com-go-logr-stdr-LICENSE](licenses/Go-github-com-go-logr-stdr-LICENSE) |
| `github.com/google/jsonschema-go` | `v0.4.3` | MIT | [Go-github-com-google-jsonschema-go-LICENSE](licenses/Go-github-com-google-jsonschema-go-LICENSE) |
| `github.com/google/uuid` | `v1.6.0` | BSD-3-Clause | [Go-github-com-google-uuid-LICENSE](licenses/Go-github-com-google-uuid-LICENSE) |
| `github.com/grafana/regexp` | `v0.0.0-20240518133315-a468a5bfb3bc` | BSD-3-Clause | [Go-github-com-grafana-regexp-LICENSE](licenses/Go-github-com-grafana-regexp-LICENSE) |
| `github.com/invopop/jsonschema` | `v0.13.0` | MIT | [Go-github-com-invopop-jsonschema-COPYING](licenses/Go-github-com-invopop-jsonschema-COPYING) |
| `github.com/jpillora/backoff` | `v1.0.0` | MIT | [Go-github-com-jpillora-backoff-LICENSE](licenses/Go-github-com-jpillora-backoff-LICENSE) |
| `github.com/mailru/easyjson` | `v0.7.7` | MIT | [Go-github-com-mailru-easyjson-LICENSE](licenses/Go-github-com-mailru-easyjson-LICENSE) |
| `github.com/modelcontextprotocol/go-sdk` | `v1.8.0` | MIT AND Apache-2.0 | [Go-github-com-modelcontextprotocol-go-sdk-LICENSE](licenses/Go-github-com-modelcontextprotocol-go-sdk-LICENSE) |
| `github.com/munnerz/goautoneg` | `v0.0.0-20191010083416-a7dc8b61c822` | BSD-3-Clause | [Go-github-com-munnerz-goautoneg-LICENSE](licenses/Go-github-com-munnerz-goautoneg-LICENSE) |
| `github.com/openai/tunnel-client` | `v0.0.14` | Apache-2.0 | [OpenAI-Tunnel-Client-LICENSE](licenses/OpenAI-Tunnel-Client-LICENSE), [OpenAI-Tunnel-Client-NOTICE](licenses/OpenAI-Tunnel-Client-NOTICE) |
| `github.com/panjf2000/ants/v2` | `v2.11.3` | MIT | [Go-github-com-panjf2000-ants-v2-LICENSE](licenses/Go-github-com-panjf2000-ants-v2-LICENSE) |
| `github.com/prometheus/client_golang` | `v1.23.2` | Apache-2.0 AND BSD-3-Clause (vendored gddo) | [Go-github-com-prometheus-client-golang-LICENSE](licenses/Go-github-com-prometheus-client-golang-LICENSE), [Go-github-com-prometheus-client-golang-NOTICE](licenses/Go-github-com-prometheus-client-golang-NOTICE), [Go-github-com-prometheus-client-golang-gddo-LICENSE](licenses/Go-github-com-prometheus-client-golang-gddo-LICENSE) |
| `github.com/prometheus/client_model` | `v0.6.2` | Apache-2.0 | [Go-github-com-prometheus-client-model-LICENSE](licenses/Go-github-com-prometheus-client-model-LICENSE), [Go-github-com-prometheus-client-model-NOTICE](licenses/Go-github-com-prometheus-client-model-NOTICE) |
| `github.com/prometheus/common` | `v0.66.1` | Apache-2.0 | [Go-github-com-prometheus-common-LICENSE](licenses/Go-github-com-prometheus-common-LICENSE), [Go-github-com-prometheus-common-NOTICE](licenses/Go-github-com-prometheus-common-NOTICE) |
| `github.com/prometheus/otlptranslator` | `v0.0.2` | Apache-2.0 | [Go-github-com-prometheus-otlptranslator-LICENSE](licenses/Go-github-com-prometheus-otlptranslator-LICENSE) |
| `github.com/prometheus/procfs` | `v0.17.0` | Apache-2.0 | [Go-github-com-prometheus-procfs-LICENSE](licenses/Go-github-com-prometheus-procfs-LICENSE), [Go-github-com-prometheus-procfs-NOTICE](licenses/Go-github-com-prometheus-procfs-NOTICE) |
| `github.com/segmentio/asm` | `v1.1.3` | MIT | [Go-github-com-segmentio-asm-LICENSE](licenses/Go-github-com-segmentio-asm-LICENSE) |
| `github.com/segmentio/encoding` | `v0.5.4` | MIT AND BSD-3-Clause (Go-derived code) | [Go-github-com-segmentio-encoding-LICENSE](licenses/Go-github-com-segmentio-encoding-LICENSE), [Go-stdlib-LICENSE](licenses/Go-stdlib-LICENSE) |
| `github.com/spf13/pflag` | `v1.0.7` | BSD-3-Clause | [Go-github-com-spf13-pflag-LICENSE](licenses/Go-github-com-spf13-pflag-LICENSE) |
| `github.com/u-root/u-root` | `v0.16.0` | BSD-3-Clause | [Go-github-com-u-root-u-root-LICENSE](licenses/Go-github-com-u-root-u-root-LICENSE) |
| `github.com/wk8/go-ordered-map/v2` | `v2.1.8` | Apache-2.0 | [Go-github-com-wk8-go-ordered-map-v2-LICENSE](licenses/Go-github-com-wk8-go-ordered-map-v2-LICENSE) |
| `github.com/yosida95/uritemplate/v3` | `v3.0.2` | BSD-3-Clause | [Go-github-com-yosida95-uritemplate-v3-LICENSE](licenses/Go-github-com-yosida95-uritemplate-v3-LICENSE) |
| `go.opentelemetry.io/auto/sdk` | `v1.2.1` | Apache-2.0 | [Go-go-opentelemetry-io-auto-sdk-LICENSE](licenses/Go-go-opentelemetry-io-auto-sdk-LICENSE) |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | `v0.63.0` | Apache-2.0 AND BSD-3-Clause | [Go-go-opentelemetry-io-contrib-instrumentation-net-http-otelhttp-LICENSE](licenses/Go-go-opentelemetry-io-contrib-instrumentation-net-http-otelhttp-LICENSE) |
| `go.opentelemetry.io/otel/exporters/prometheus` | `v0.60.0` | Apache-2.0 AND BSD-3-Clause | [Go-go-opentelemetry-io-otel-exporters-prometheus-LICENSE](licenses/Go-go-opentelemetry-io-otel-exporters-prometheus-LICENSE) |
| `go.opentelemetry.io/otel/metric` | `v1.41.0` | Apache-2.0 AND BSD-3-Clause | [Go-go-opentelemetry-io-otel-metric-LICENSE](licenses/Go-go-opentelemetry-io-otel-metric-LICENSE) |
| `go.opentelemetry.io/otel/sdk/metric` | `v1.41.0` | Apache-2.0 AND BSD-3-Clause | [Go-go-opentelemetry-io-otel-sdk-metric-LICENSE](licenses/Go-go-opentelemetry-io-otel-sdk-metric-LICENSE) |
| `go.opentelemetry.io/otel/sdk` | `v1.41.0` | Apache-2.0 AND BSD-3-Clause | [Go-go-opentelemetry-io-otel-sdk-LICENSE](licenses/Go-go-opentelemetry-io-otel-sdk-LICENSE) |
| `go.opentelemetry.io/otel/trace` | `v1.41.0` | Apache-2.0 AND BSD-3-Clause | [Go-go-opentelemetry-io-otel-trace-LICENSE](licenses/Go-go-opentelemetry-io-otel-trace-LICENSE) |
| `go.opentelemetry.io/otel` | `v1.41.0` | Apache-2.0 AND BSD-3-Clause | [Go-go-opentelemetry-io-otel-LICENSE](licenses/Go-go-opentelemetry-io-otel-LICENSE) |
| `go.uber.org/dig` | `v1.19.0` | MIT | [Go-go-uber-org-dig-LICENSE](licenses/Go-go-uber-org-dig-LICENSE) |
| `go.uber.org/fx` | `v1.23.0` | MIT | [Go-go-uber-org-fx-LICENSE](licenses/Go-go-uber-org-fx-LICENSE) |
| `go.uber.org/multierr` | `v1.11.0` | MIT | [Go-go-uber-org-multierr-LICENSE.txt](licenses/Go-go-uber-org-multierr-LICENSE.txt) |
| `go.uber.org/zap` | `v1.27.0` | MIT | [Go-go-uber-org-zap-LICENSE](licenses/Go-go-uber-org-zap-LICENSE) |
| `go.yaml.in/yaml/v2` | `v2.4.2` | MIT AND Apache-2.0 | [Go-go-yaml-in-yaml-v2-LICENSE](licenses/Go-go-yaml-in-yaml-v2-LICENSE), [Go-go-yaml-in-yaml-v2-LICENSE.libyaml](licenses/Go-go-yaml-in-yaml-v2-LICENSE.libyaml), [Go-go-yaml-in-yaml-v2-NOTICE](licenses/Go-go-yaml-in-yaml-v2-NOTICE) |
| `golang.org/x/crypto` | `v0.51.0` | BSD-3-Clause | [Go-golang-org-x-crypto-LICENSE](licenses/Go-golang-org-x-crypto-LICENSE) |
| `golang.org/x/image` | `v0.46.0` | BSD-3-Clause | [Go-golang-org-x-image-LICENSE](licenses/Go-golang-org-x-image-LICENSE) |
| `golang.org/x/net` | `v0.53.0` | BSD-3-Clause | [Go-golang-org-x-net-LICENSE](licenses/Go-golang-org-x-net-LICENSE) |
| `golang.org/x/oauth2` | `v0.35.0` | BSD-3-Clause | [Go-golang-org-x-oauth2-LICENSE](licenses/Go-golang-org-x-oauth2-LICENSE) |
| `golang.org/x/sync` | `v0.20.0` | BSD-3-Clause | [Go-golang-org-x-sync-LICENSE](licenses/Go-golang-org-x-sync-LICENSE) |
| `golang.org/x/sys` | `v0.48.0` | BSD-3-Clause | [Go-golang-org-x-sys-LICENSE](licenses/Go-golang-org-x-sys-LICENSE) |
| `golang.org/x/time` | `v0.15.0` | BSD-3-Clause | [Go-golang-org-x-time-LICENSE](licenses/Go-golang-org-x-time-LICENSE) |
| `google.golang.org/protobuf` | `v1.36.8` | BSD-3-Clause | [Go-google-golang-org-protobuf-LICENSE](licenses/Go-google-golang-org-protobuf-LICENSE) |
| `gopkg.in/yaml.v3` | `v3.0.1` | MIT AND Apache-2.0 | [Go-gopkg-in-yaml-v3-LICENSE](licenses/Go-gopkg-in-yaml-v3-LICENSE), [Go-gopkg-in-yaml-v3-NOTICE](licenses/Go-gopkg-in-yaml-v3-NOTICE) |

## Rust Windows worker dependencies

The Windows `lrmcp.exe` embeds the native Computer worker built with Rust 1.96.0 for `x86_64-pc-windows-msvc`. The crate inventory below follows `Cargo.lock` and the target's normal dependency graph (`cargo tree --locked --offline --target x86_64-pc-windows-msvc --edges normal,no-proc-macro`). Build-only procedural macros and other platform dependencies are outside this runtime inventory.

For crates offering compatible alternatives, this distribution uses the license shown in the table. Original license texts, accompanying notices, and additional source-file copyright declarations are retained. The packaged `windows-capture 2.0.1` omitted its license file; its MIT text is reproduced from the [upstream commit recorded by that crate](https://raw.githubusercontent.com/NiiightmareXD/windows-capture/c7d106448eb9d9b251345c39047711e1cd408ae2/LICENCE).

| Component | Version | Selected license | Included license and notice files |
| --- | --- | --- | --- |
| `adler2` | `2.0.1` | MIT | [Rust-adler2-2.0.1-LICENSE](licenses/Rust-adler2-2.0.1-LICENSE) |
| `base64` | `0.22.1` | MIT | [Rust-base64-0.22.1-LICENSE](licenses/Rust-base64-0.22.1-LICENSE) |
| `bitflags` | `2.13.2` | MIT | [Rust-bitflags-2.13.2-LICENSE](licenses/Rust-bitflags-2.13.2-LICENSE), [Rust-bitflags-2.13.2-NOTICE](licenses/Rust-bitflags-2.13.2-NOTICE) |
| `bytemuck` | `1.25.2` | MIT | [Rust-bytemuck-1.25.2-LICENSE](licenses/Rust-bytemuck-1.25.2-LICENSE) |
| `byteorder-lite` | `0.1.0` | MIT | [Rust-byteorder-lite-0.1.0-LICENSE](licenses/Rust-byteorder-lite-0.1.0-LICENSE) |
| `cfg-if` | `1.0.4` | MIT | [Rust-cfg-if-1.0.4-LICENSE](licenses/Rust-cfg-if-1.0.4-LICENSE) |
| `crc32fast` | `1.5.2` | MIT | [Rust-crc32fast-1.5.2-LICENSE](licenses/Rust-crc32fast-1.5.2-LICENSE) |
| `crossbeam-deque` | `0.8.8` | MIT | [Rust-crossbeam-deque-0.8.8-LICENSE](licenses/Rust-crossbeam-deque-0.8.8-LICENSE) |
| `crossbeam-epoch` | `0.9.21` | MIT | [Rust-crossbeam-epoch-0.9.21-LICENSE](licenses/Rust-crossbeam-epoch-0.9.21-LICENSE) |
| `crossbeam-utils` | `0.8.23` | MIT | [Rust-crossbeam-utils-0.8.23-LICENSE](licenses/Rust-crossbeam-utils-0.8.23-LICENSE) |
| `either` | `1.18.0` | MIT | [Rust-either-1.18.0-LICENSE](licenses/Rust-either-1.18.0-LICENSE) |
| `fdeflate` | `0.3.7` | MIT | [Rust-fdeflate-0.3.7-LICENSE](licenses/Rust-fdeflate-0.3.7-LICENSE) |
| `flate2` | `1.1.10` | MIT | [Rust-flate2-1.1.10-LICENSE](licenses/Rust-flate2-1.1.10-LICENSE), [Rust-flate2-1.1.10-NOTICE](licenses/Rust-flate2-1.1.10-NOTICE) |
| `image` | `0.25.10` | MIT | [Rust-image-0.25.10-LICENSE](licenses/Rust-image-0.25.10-LICENSE), [Rust-image-0.25.10-NOTICE](licenses/Rust-image-0.25.10-NOTICE) |
| `itoa` | `1.0.18` | MIT | [Rust-itoa-1.0.18-LICENSE](licenses/Rust-itoa-1.0.18-LICENSE) |
| `lock_api` | `0.4.14` | MIT | [Rust-lock_api-0.4.14-LICENSE](licenses/Rust-lock_api-0.4.14-LICENSE), [Rust-lock_api-0.4.14-NOTICE](licenses/Rust-lock_api-0.4.14-NOTICE) |
| `memchr` | `2.8.3` | MIT | [Rust-memchr-2.8.3-COPYING](licenses/Rust-memchr-2.8.3-COPYING), [Rust-memchr-2.8.3-LICENSE](licenses/Rust-memchr-2.8.3-LICENSE) |
| `miniz_oxide` | `0.8.9` | MIT | [Rust-miniz_oxide-0.8.9-LICENSE](licenses/Rust-miniz_oxide-0.8.9-LICENSE) |
| `miniz_oxide` | `0.9.1` | MIT | [Rust-miniz_oxide-0.9.1-LICENSE](licenses/Rust-miniz_oxide-0.9.1-LICENSE) |
| `moxcms` | `0.8.1` | BSD-3-Clause | [Rust-moxcms-0.8.1-LICENSE](licenses/Rust-moxcms-0.8.1-LICENSE), [Rust-moxcms-0.8.1-NOTICE](licenses/Rust-moxcms-0.8.1-NOTICE) |
| `num-traits` | `0.2.19` | MIT | [Rust-num-traits-0.2.19-LICENSE](licenses/Rust-num-traits-0.2.19-LICENSE), [Rust-num-traits-0.2.19-NOTICE](licenses/Rust-num-traits-0.2.19-NOTICE) |
| `parking_lot` | `0.12.5` | MIT | [Rust-parking_lot-0.12.5-LICENSE](licenses/Rust-parking_lot-0.12.5-LICENSE), [Rust-parking_lot-0.12.5-NOTICE](licenses/Rust-parking_lot-0.12.5-NOTICE) |
| `parking_lot_core` | `0.9.12` | MIT | [Rust-parking_lot_core-0.9.12-LICENSE](licenses/Rust-parking_lot_core-0.9.12-LICENSE), [Rust-parking_lot_core-0.9.12-NOTICE](licenses/Rust-parking_lot_core-0.9.12-NOTICE) |
| `png` | `0.18.1` | MIT | [Rust-png-0.18.1-LICENSE](licenses/Rust-png-0.18.1-LICENSE) |
| `pxfm` | `0.1.30` | BSD-3-Clause | [Rust-pxfm-0.1.30-LICENSE](licenses/Rust-pxfm-0.1.30-LICENSE), [Rust-pxfm-0.1.30-NOTICE](licenses/Rust-pxfm-0.1.30-NOTICE) |
| `rayon` | `1.12.0` | MIT | [Rust-rayon-1.12.0-LICENSE](licenses/Rust-rayon-1.12.0-LICENSE) |
| `rayon-core` | `1.13.0` | MIT | [Rust-rayon-core-1.13.0-LICENSE](licenses/Rust-rayon-core-1.13.0-LICENSE) |
| `scopeguard` | `1.2.0` | MIT | [Rust-scopeguard-1.2.0-LICENSE](licenses/Rust-scopeguard-1.2.0-LICENSE) |
| `serde` | `1.0.229` | MIT | [Rust-serde-1.0.229-LICENSE](licenses/Rust-serde-1.0.229-LICENSE) |
| `serde_core` | `1.0.229` | MIT | [Rust-serde_core-1.0.229-LICENSE](licenses/Rust-serde_core-1.0.229-LICENSE) |
| `serde_json` | `1.0.151` | MIT | [Rust-serde_json-1.0.151-LICENSE](licenses/Rust-serde_json-1.0.151-LICENSE), [Rust-serde_json-1.0.151-NOTICE](licenses/Rust-serde_json-1.0.151-NOTICE) |
| `simd-adler32` | `0.3.10` | MIT | [Rust-simd-adler32-0.3.10-LICENSE](licenses/Rust-simd-adler32-0.3.10-LICENSE) |
| `smallvec` | `1.16.1` | MIT | [Rust-smallvec-1.16.1-LICENSE](licenses/Rust-smallvec-1.16.1-LICENSE) |
| `thiserror` | `2.0.20` | MIT | [Rust-thiserror-2.0.20-LICENSE](licenses/Rust-thiserror-2.0.20-LICENSE) |
| `windows` | `0.62.2` | MIT | [Rust-windows-0.62.2-LICENSE](licenses/Rust-windows-0.62.2-LICENSE) |
| `windows-capture` | `2.0.1` | MIT | [Rust-windows-capture-2.0.1-LICENSE](licenses/Rust-windows-capture-2.0.1-LICENSE) |
| `windows-collections` | `0.3.2` | MIT | [Rust-windows-collections-0.3.2-LICENSE](licenses/Rust-windows-collections-0.3.2-LICENSE) |
| `windows-core` | `0.62.2` | MIT | [Rust-windows-core-0.62.2-LICENSE](licenses/Rust-windows-core-0.62.2-LICENSE) |
| `windows-future` | `0.3.2` | MIT | [Rust-windows-future-0.3.2-LICENSE](licenses/Rust-windows-future-0.3.2-LICENSE) |
| `windows-link` | `0.2.1` | MIT | [Rust-windows-link-0.2.1-LICENSE](licenses/Rust-windows-link-0.2.1-LICENSE) |
| `windows-numerics` | `0.3.1` | MIT | [Rust-windows-numerics-0.3.1-LICENSE](licenses/Rust-windows-numerics-0.3.1-LICENSE) |
| `windows-result` | `0.4.1` | MIT | [Rust-windows-result-0.4.1-LICENSE](licenses/Rust-windows-result-0.4.1-LICENSE) |
| `windows-strings` | `0.5.1` | MIT | [Rust-windows-strings-0.5.1-LICENSE](licenses/Rust-windows-strings-0.5.1-LICENSE) |
| `windows-threading` | `0.2.1` | MIT | [Rust-windows-threading-0.2.1-LICENSE](licenses/Rust-windows-threading-0.2.1-LICENSE) |
| `zmij` | `1.0.23` | MIT | [Rust-zmij-1.0.23-LICENSE](licenses/Rust-zmij-1.0.23-LICENSE) |

The Rust standard library and runtime use MIT or Apache-2.0, with bundled components retaining their additional licenses. The complete Rust 1.96.0 standard-library copyright inventory is reproduced alongside its license texts. This upstream inventory includes other target platforms' attribution; its presence does not imply that those platform components are linked into the Windows worker.

| Component | Version | Applicable license | Included license and notice files |
| --- | --- | --- | --- |
| Rust standard library and runtime | `1.96.0` | MIT OR Apache-2.0; bundled BSD-2-Clause and Unicode-3.0 components | [Rust-stdlib-COPYRIGHT-library.html](licenses/Rust-stdlib-COPYRIGHT-library.html), [Rust-stdlib-LICENSE-MIT](licenses/Rust-stdlib-LICENSE-MIT), [Rust-stdlib-LICENSE-Apache-2.0](licenses/Rust-stdlib-LICENSE-Apache-2.0), [Rust-stdlib-LICENSE-BSD-2-Clause](licenses/Rust-stdlib-LICENSE-BSD-2-Clause), [Rust-stdlib-LICENSE-Unicode-3.0](licenses/Rust-stdlib-LICENSE-Unicode-3.0) |
| `compiler_builtins` and compiler-rt contributions | `0.1.160` (Rust 1.96.0) | MIT AND Apache-2.0 WITH LLVM-exception | [Rust-stdlib-compiler-builtins-LICENSE](licenses/Rust-stdlib-compiler-builtins-LICENSE), [Rust-stdlib-compiler-rt-CREDITS](licenses/Rust-stdlib-compiler-rt-CREDITS) |
| `libm` and bundled math contributions | `0.2.16` (Rust 1.96.0) | MIT, with bundled terms retained | [Rust-stdlib-libm-LICENSE](licenses/Rust-stdlib-libm-LICENSE) |

## Microsoft native runtime

The embedded Windows worker is built with the Microsoft MSVC toolchain and statically links the compiler and C runtime components selected by Rust's `crt-static` option. Microsoft runtime code retains its Microsoft licensing. Redistribution is governed by the applicable Microsoft Software License Terms and REDIST list; see the official [Visual C++ redistribution guide](https://learn.microsoft.com/en-us/cpp/windows/redistributing-visual-cpp-files?view=msvc-170), [Visual Studio license directory](https://visualstudio.microsoft.com/license-terms/), and [Visual Studio 2022 distributable code list](https://learn.microsoft.com/en-us/visualstudio/releases/2022/redistribution).

## Cloudflare Tunnel

Release archives include the unmodified `cloudflared` companion binary, version 2026.9.1, from the [Cloudflare Tunnel project](https://github.com/cloudflare/cloudflared).

`cloudflared` is a separate program distributed under the Apache License 2.0. Local Runtime MCP invokes it as a child process and does not link or incorporate its source code. Its license is included in release archives as `cloudflared-LICENSE`.
