# 连接与 OAuth

[English](connections.md) · **简体中文**

一个 `lrmcp` 进程管理一个 Runtime Host。所有入口提供相同的 21 个工具，共享进程会话、浏览器目标、文件提交协调和桌面控制。

## 选择传输与 Provider

```powershell
# 本地进程集成
./lrmcp.exe serve --transport stdio

# 直接访问 loopback HTTP
./lrmcp.exe serve --transport http

# 单个远端 Provider
./lrmcp.exe serve --transport http --tunnel openai
./lrmcp.exe serve --transport http --tunnel cloudflare

# 两个远端 Provider，共享一个 Host
./lrmcp.exe serve --transport http --tunnel openai --tunnel cloudflare
```

传输方式需要明确选择，Tunnel 负责转发 HTTP。`local-runtime-mcp.yaml` 中保存的凭据在选中对应 Provider 时生效。仅使用 OpenAI 时创建私有 HTTP 源站；直接 HTTP 或 Cloudflare 使用配置中的公开访问源站；双 Provider 在同一个 MCP Server 上创建两种源站。

每个 Provider 管理自己的连接生命周期。某个 Provider 发生终止错误会明确报告，其他在线入口继续服务。按 `Ctrl+C` 关闭整个运行时；所有入口结束时 Host 退出并报告对应失败。恢复可以重新建立传输，机器操作是否重试由客户端明确决定。

## 凭据职责

| 凭据 | 使用者 | 权限 |
| --- | --- | --- |
| OpenAI Tunnel ID 与 API key | OpenAI 转发客户端 | 连接对应 OpenAI Tunnel |
| Cloudflare Tunnel token | `cloudflared` | 连接对应 Cloudflare Tunnel |
| HTTP Bearer token | Bearer 模式下的 MCP 客户端 | 调用完整运行时工具 |
| OAuth client ID 与 secret | 注册的 MCP 客户端授权流程 | 标识并认证该客户端 |
| 所有者批准密钥 | 授权页面上的机器操作者 | 批准请求的运行时访问 |
| OAuth 访问 token 与刷新 token | 已授权 MCP 客户端 | 调用或续期运行时访问 |
| Browser Bridge token | Chromium 扩展 | 将该 Profile 连接到本地 Bridge |

需要持久化的凭据保存在程序同目录。准备命令在本地生成 OAuth 凭据。妥善保护配置、生成的扩展设置、终端输出和移动存储。

## Gemini 配置

1. 创建 Cloudflare 管理的 Tunnel 和公开域名，例如 `mcp.example.com`。将整个域名路由到 loopback 源站，通常为 `http://127.0.0.1:9316`，使 MCP 路径和 OAuth 发现、授权、token 路径均可访问。
2. 在程序同目录的 `local-runtime-mcp.yaml` 中填写 `http.public_host` 与 `cloudflare.tunnel_token`。
3. 在 Gemini 自定义 MCP 连接窗口填写 `https://mcp.example.com/mcp`，展开附加设置，复制 Gemini 提供的 redirect URI。
4. 在本地注册准确地址：

   ```powershell
   ./lrmcp.exe setup oauth --client-name gemini --redirect-uri "https://<复制的回调域名>/<复制的回调路径>"
   ./lrmcp.exe setup oauth --client-name gemini --show
   ```

5. 将生成的 client ID 和 client secret 填入 Gemini 对应字段。保留所有者批准密钥用于授权确认。准备命令选择 `http.auth: oauth`，凭据保存在同目录配置。
6. 开启 Cloudflare，或者同时启用两个 Provider：

   ```powershell
   ./lrmcp.exe serve --transport http --tunnel openai --tunnel cloudflare
   ```

7. 继续客户端连接流程。在 Local Runtime 授权页面核对公开域名、注册客户端和访问范围，使用所有者批准密钥确认。浏览器经注册回调返回客户端应用，随后完成令牌交换和工具发现。
8. 确认客户端发现 21 个工具。先验证只读版本查询、浏览器状态和文件元数据，再验证有副作用的操作。

回调 URI 需要准确匹配，包括路径和查询参数。同一客户端需要多个允许回调时重复传入 `--redirect-uri`。再次为同名客户端执行准备命令会追加回调，保留已有凭据。客户端名称就是 client ID，默认为 `gemini`。注册和 `--show` 都会在本地显示该客户端的凭据。

本地协议测试验证 OAuth 实现。Gemini 实际验收还取决于当前 Host 集成、账户是否开放自定义应用、复制的回调和公网路由，应在真实客户端中确认这些条件。

## 授权生命周期

公开源站选择 Bearer 或 OAuth 模式。OAuth 使用 Authorization Code 流程、S256 PKCE、准确回调匹配、所有者批准和刷新 token 轮换。OpenAI 私有链路凭据在进程启动时生成，与公开认证独立。

OAuth 授权开放完整机器控制能力，权限等于运行 `lrmcp` 的账户。资源 ID 和桌面控制权在这套共享权限内协调任务；需要独立安全边界时使用不同操作系统账户或机器。

授权码、待批准请求、访问 token 和刷新 token 保存在内存中，重启运行时后客户端需要重新授权。注册客户端凭据和所有者批准密钥保留在便携 YAML 中。更新程序保留操作者的配置，认证模式切换由准备命令或配置修改明确触发。

## 跨 Provider 验收

使用一个经 OpenAI 连接的客户端和一个经 Cloudflare 连接的客户端验证同一个 Host。各工具单独调用，记录结果和时间。报告中隐去凭据、控制 token、截图和私密页面内容。

| 验收项 | 证据 |
| --- | --- |
| 同一个运行时 | 两端取得同一个程序版本，并发现 21 个工具。 |
| 共享进程注册表 | A 创建带独特标记的短异步进程，只把 `session_id` 交给 B。B 用明确游标调用 `process_continue` 读取；A 仍能重读保留输出。 |
| 共享浏览器注册表 | 两端看到相同在线浏览器实例 ID，并能明确选择不同实例或已有标签页。 |
| 共享桌面协调 | A 用测试标签获取控制权；A 持有期间 B 获取得到 busy，状态显示 A 的标签；A 只释放自己的 token。 |
| 独立进程工作 | [双会话提示词](concurrency-acceptance.zh-CN.md)得到时间区间重叠、标记分别纯净、游标采集完整的进程输出。 |
| 原生图像结果 | 分别检查 image block 与结构化元数据；仅有 MIME 和尺寸不能证明 Host 已把像素交给模型。 |

跨客户端读取进程是有意证明共享注册表，并发协调与独立租户隔离属于不同验收。各客户端只清理自己创建的进程或取得的控制权。缺少浏览器 Profile 或图像路径不可访问时标记未覆盖。

## 多台机器

每台机器运行独立 Host，使用可以明确区分的连接身份和公网地址。进程会话、浏览器 Profile 与交互桌面属于对应机器。Cloudflare 同一 Tunnel 的 replicas 用于可互换的源站；不同机器具有独立资源 ID 时，使用独立 Tunnel 或明确路由。
