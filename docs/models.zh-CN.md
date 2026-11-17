# Local Runtime：工具、网页与模型

Local Runtime 是同一个本地运行时的三种入口。MCP 让 Agent 调用本机和已注册网页能力；网页 SDK 让已授权的网站调用有限能力；模型接口让 Harness 和原生应用使用浏览器里的模型。运行时不接管 Agent 循环：工具执行、结果回传和下一步决策仍归 Harness 或应用。

```mermaid
flowchart TB
    EXP["探索网页：复用浏览器登录态"] --> FILE["能力文件：同名 JSON + JS"]
    FILE --> RT["一个 Local Runtime"]
    RT --> MCP["MCP 工具"]
    RT --> WEB["网页 SDK · 按来源授权"]
    RT --> API["本地模型 API · 独立 Key"]
    START["模型网页的 start / read / stop 能力"] --> RT
    API --> HARNESS["Harness：工具请求 → 执行 → 回传"]
    WEB --> APP["StuDev Chat：查询校园事实 → 回答"]
```

## 能力注册

`capabilities/deepseek/start.json` 和 `start.js` 定义同一个能力 `deepseek.start`；另外两对文件定义读取和停止。文件树就是注册表，启动时读取并校验，没有额外清单或数据库。JSON 描述来源范围、输入输出和副作用；JS 是接收 `input` 的完整函数表达式。详见 [能力目录说明](../capabilities/README.md)。

新增网站时，先用浏览器工具探索真实页面，再保存经过验证的原子能力。MOOC 是这种能力的一个应用场景，不是运行时的业务模型。不要把具体课程、校园事实或 Agent 任务循环放入通用内核。

## 配置与启动

参照 [配置示例](../config.example.yaml) 配置 `capabilities` 和 `models`。目录相对配置文件解析；发布包附带能力文件。只有 `capabilities.web` 中列出的精确来源可以使用网页桥；网页只能指定能力名和输入。目标网址由本机配置确定，网页不能指定其他标签页、浏览器或任意脚本。

`models.key` 与浏览器连接凭据、MCP 凭据必须不同。也可在单次子进程中提供 `LOCAL_RUNTIME_MCP_MODEL_KEY`。不要把模型 Key 放入网站代码；网站通过扩展 SDK 访问。

```text
lrmcp serve --transport http
GET  http://127.0.0.1:9316/v1/models
POST http://127.0.0.1:9316/v1/chat/completions
Authorization: Bearer <model-only-key>
```

MCP 仍位于同一监听器的 `/mcp`，沿用原有独立认证。没有新增常驻服务、云项目或域名。默认只监听回环地址；已有 HTTPS 隧道可以显式承载远程客户端。

## 模型与工具闭环

调用方用稳定的 `conversation_id` 指定会话：Harness 使用自己的 Session ID，StuDev 使用当前聊天分支的 ID。第一次为它创建专用网页会话，后续消息和工具结果在同一个会话中追加。网页来源、模型和会话标识共同确定资源归属；不同网站不能用相同 ID 接续彼此的聊天。运行时不读用户未指定的旧聊天，不提取登录 Cookie，也不将网页凭据交给调用者。

同一会话串行执行，返回结果携带 `conversation_id`。调用方切换历史分支时显式使用新的分支 ID；上下文前缀不一致会返回冲突，不会悄悄改用别的聊天。失败或取消只停止当前轮次，保持该会话；重试须由调用方明确发起。`DELETE /v1/conversations/<id>?model=<model>` 或 SDK 的关闭操作结束运行时自己的会话资源，不删除模型网站上的聊天记录。

普通回答可以流式返回。带工具时，网页模型必须返回完整的结构化 JSON，运行时验证工具名、参数 schema 和工具选择要求后才转成标准 `tool_calls`。运行时不执行这些工具。Harness 或 StuDev 执行后，在下一次请求中携带工具结果。原始 DSML、Markdown 代码块和未通过校验的参数不能成为工具调用。

取消和超时会请求停止本轮网页生成；轮次标识防止迟到的取消影响下一轮。发送结果不明确时不自动重放请求，也不擅自创建替代聊天。错误不会伪装成正常完成。

网页模型不提供可验证的采样、精确 token 上限、图片或完整 OpenAI 功能。本接口明确拒绝未实现的请求参数；不要把它当作官方 DeepSeek API。客户端适配应只提交支持的字段，不应偷偷忽略参数。Harness 集成使用官方 `LlmAdapter` 接口，避免通用供应商适配器强加网页无法控制的默认参数。
