# 双会话并发验收

用两个同时运行的 ChatGPT 会话验证同一个 Local Runtime MCP Host。测试覆盖浏览器空闲连接、进程输出分流、Profile 路由和桌面控制权。

## 准备

- 更新运行时和扩展，在各个目标 Profile 中重新加载扩展，填写不同名称并保留普通网页标签。
- 保持浏览器和运行时打开，关闭扩展 Worker 的调试窗口。重新加载后先进行本测试，再使用页面快照或其他 debugger 工具，以形成空闲连接测试的基线。
- 两个云端会话连接同一个 Host，刷新工具目录。Windows 机器需要已安装 `pwsh.exe`，并可通过 PATH 执行 `lrmcp`。
- 同时把下列 A、B 提示词分别发给两个会话。至少两个在线 Profile 才能覆盖跨 Profile 路由；只有一个时仍可验证其他项目。

控制权申请会显示本地提示层；以下测试保持页面和文件只读，不产生桌面鼠标、键盘输入。控制 token 仅用于工具参数，报告中应隐去。

v11 通过 `output_cursor` 读取进程增量输出。只在成功收到结果后推进游标；纯读取失败时使用同一游标重试，避免漏读或重复拼接。已完成的异步会话在保留期内也可以重读。首次启动、stdin 输入与其他有副作用请求的结果不确定时，先检查资源状态。

## 会话 A 提示词

```text
你是会话 A。请实测 Local Runtime MCP 的双会话并发，另一个会话 B 同时测试。只使用这个 MCP，以当前公开工具 schema 为准。各工具单独调用，以便明确异常对应的工具和时间。完整执行后报告证据。

约束：不修改任何文件；不调用 browser_open、browser_close、browser_navigate、browser_action、browser_screenshot 或 computer_action；不用其他工具、MCP、调试窗口或 JavaScript 探针。只收集和清理自己创建的 process session、自己取得的 control_id。不要在回复中公开 control_id、配置凭据或私密页面内容。

1. 调用 browser_status。先筛选 connected=true 的 instances，再按 browser_id 字符串升序排序，选择第一个并固定。记录该实例的 browser_id、label、extension_version、last_seen。没有在线实例则记录浏览器未覆盖，继续其他测试。后续该实例离线时报告，不改选其他实例。

2. 测桌面竞争：用 process_run(program="pwsh.exe", args=["-NoProfile","-Command","Get-Date -Format o"], yield_time_ms=1000) 获取机器时间，随后调用 computer_control(kind="acquire", label="LRMCP 并发 A")，再获取一次机器时间，形成申请时间区间。成功时私密保留自己的 control_id，持有到本轮结束；busy 时立即调用 computer_control(kind="status")，记录 status、label、expires_at 和申请时间，既不抢占也不释放别人的控制权。busy 且 label 是 LRMCP 并发 B 是竞争证据；其他占用名称按实际记录。

3. 用 process_run 启动自己的 120 秒进程：
   program="pwsh.exe", io_mode="pipe", yield_time_ms=1000, timeout_seconds=240,
   args=["-NoProfile","-Command","$ErrorActionPreference='Stop'; 1..120 | ForEach-Object { Write-Output ('GPT-A tick={0} time={1}' -f $_,(Get-Date -Format o)); Start-Sleep -Seconds 1 }"]。
   保存自己的 session_id、首次结果的 output_cursor 与第一条 tick 时间；在后续调用中只能使用这个 session_id。

4. 前 90 秒是浏览器空闲观察阶段：只调用 browser_status，以及 process_continue(session_id=自己的ID, output_cursor=最后成功结果的游标, yield_time_ms=10000) 推进等待并收集 tick。成功时才拼接本次输出并推进游标；纯读失败可用同一游标重试一次，记录原错误和单个工具名称，不重启或重发 process_run。先调用 browser_status，再交替执行一次 continue 和一次 status。以 tick 的机器时间和首条时间的差判断是否已过 90 秒，不能只按调用次数计时。不要调用 browser_tabs、browser_snapshot，也不要做任何会建立 debugger 会话的操作。
   每次记录选定实例的 connected、last_seen，以及本次最新 tick 时间。检查 last_seen 是否持续推进；若失联，保留前后证据并继续其他项目。status 本身只读取 Host 状态，不是扩展心跳来源。

5. 空闲窗口达到至少 90 秒后：
   - 若选定实例仍在线，调用 browser_tabs(browser_id=固定ID)。选择已有普通 http/https 网页，复制返回的 tab.id 作为 tab_id，调用 browser_snapshot(tab_id=..., max_elements=20, max_text=1000)。核对 tab.browser_id 与固定ID一致，快照 tab_id 与所选标签一致。没有可用网页则标记未覆盖，不打开新标签。
   - 调用 process_run(program="lrmcp", args=["version"], yield_time_ms=1000)。
   - 用一个只读 pwsh 进程定位测试文件：通过 Get-Command lrmcp -CommandType Application 获取程序目录，Join-Path 定位同目录 README.md；通过 $env:WINDIR 定位 Web\Wallpaper\Windows\img0.jpg。只输出这些候选路径及是否存在，不读取配置、环境凭据或其他私密文件。
   - 对实际存在的 README 调用 filesystem_stat 和 filesystem_read_text(max_bytes=1024)。对实际存在的壁纸调用 image_read。不存在则标记未覆盖，不搜索个人文件。
   - 调用 computer_targets，再对已有目标调用 computer_state，省略 control_id，验证只读 actionable=false、state_id 为空。不要激活窗口或输入。
   - 再次调用 browser_status，记录选定实例连接状态及 last_seen。

6. 继续按最后成功结果的 output_cursor 采集自己 120 秒进程，直到 running=false。合并所有成功读取的增量输出，检查 tick 1..120 是否完整、是否只有 GPT-A 标记。记录首尾机器时间、最终 duration_ms、exit_code、stderr、timed_out、截断标志与实际采集时间。随后用最终游标做一次纯读，确认退出状态仍可读取且没有新输出；该结果不再拼接。duration_ms 是进程运行时长，不是你完成全部工具调用的耗时；较晚采集完成结果时也应约为 120 秒，调度可能造成少量偏差。

7. finally：成功取得控制权才用自己的 control_id release，并查询 status 确认清理结果；不释放任何其他 token。遇到异常时，对自己仍运行的测试进程使用 process_continue(terminate=true)，并记录清理结果。短进程若也返回 session_id，同样只清理自己的。

报告：版本；每项 PASS/FAIL/未覆盖及错误原文；每次浏览器 status 的 tick 时间、browser_id、connected、last_seen；进程首尾时间区间和输出完整性；桌面 acquire 时间区间、busy 时的占用 label、自己的释放结果。可以展示 process session_id、browser_id、tab_id，但不要展示 control_id。只摘要网页与图像元数据。
单边报告只证明你实际执行的项目；两份进程时间区间重叠且输出各自纯净才能证明进程并发；busy 同时指向另一任务标签才能支持桌面互斥；两个不同在线 browser_id 的标签与快照都成功才覆盖跨 Profile 路由。不能把未覆盖算成通过，也不能把正常 busy 算成故障。
```

## 会话 B 提示词

```text
你是会话 B。请实测 Local Runtime MCP 的双会话并发，另一个会话 A 同时测试。只使用这个 MCP，以当前公开工具 schema 为准。各工具单独调用，以便明确异常对应的工具和时间。完整执行后报告证据。

约束：不修改任何文件；不调用 browser_open、browser_close、browser_navigate、browser_action、browser_screenshot 或 computer_action；不用其他工具、MCP、调试窗口或 JavaScript 探针。只收集和清理自己创建的 process session、自己取得的 control_id。不要在回复中公开 control_id、配置凭据或私密页面内容。

1. 调用 browser_status。先筛选 connected=true 的 instances，再按 browser_id 字符串升序排序，选择第二个并固定。记录该实例的 browser_id、label、extension_version、last_seen。少于两个在线实例则记录跨 Profile 路由未覆盖，不使用第一个替代，但继续其他测试。后续该实例离线时报告，不改选其他实例。

2. 测桌面竞争：用 process_run(program="pwsh.exe", args=["-NoProfile","-Command","Get-Date -Format o"], yield_time_ms=1000) 获取机器时间，随后调用 computer_control(kind="acquire", label="LRMCP 并发 B")，再获取一次机器时间，形成申请时间区间。成功时私密保留自己的 control_id，持有到本轮结束；busy 时立即调用 computer_control(kind="status")，记录 status、label、expires_at 和申请时间，既不抢占也不释放别人的控制权。busy 且 label 是 LRMCP 并发 A 是竞争证据；其他占用名称按实际记录。

3. 用 process_run 启动自己的 120 秒进程：
   program="pwsh.exe", io_mode="pipe", yield_time_ms=1000, timeout_seconds=240,
   args=["-NoProfile","-Command","$ErrorActionPreference='Stop'; 1..120 | ForEach-Object { Write-Output ('GPT-B tick={0} time={1}' -f $_,(Get-Date -Format o)); Start-Sleep -Seconds 1 }"]。
   保存自己的 session_id、首次结果的 output_cursor 与第一条 tick 时间；在后续调用中只能使用这个 session_id。

4. 前 90 秒是浏览器空闲观察阶段：只调用 browser_status，以及 process_continue(session_id=自己的ID, output_cursor=最后成功结果的游标, yield_time_ms=10000) 推进等待并收集 tick。成功时才拼接本次输出并推进游标；纯读失败可用同一游标重试一次，记录原错误和单个工具名称，不重启或重发 process_run。先调用 browser_status，再交替执行一次 continue 和一次 status。以 tick 的机器时间和首条时间的差判断是否已过 90 秒，不能只按调用次数计时。不要调用 browser_tabs、browser_snapshot，也不要做任何会建立 debugger 会话的操作。
   每次记录选定实例的 connected、last_seen，以及本次最新 tick 时间。没有选定实例时也记录 status 中在线实例的数量，继续进程等待。检查 last_seen 是否持续推进；若失联，保留前后证据并继续其他项目。status 本身只读取 Host 状态，不是扩展心跳来源。

5. 空闲窗口达到至少 90 秒后：
   - 若选定实例仍在线，调用 browser_tabs(browser_id=固定ID)。选择已有普通 http/https 网页，复制返回的 tab.id 作为 tab_id，调用 browser_snapshot(tab_id=..., max_elements=20, max_text=1000)。核对 tab.browser_id 与固定ID一致，快照 tab_id 与所选标签一致。没有可用网页则标记未覆盖，不打开新标签。
   - 调用 process_run(program="lrmcp", args=["version"], yield_time_ms=1000)。
   - 用一个只读 pwsh 进程定位测试文件：通过 Get-Command lrmcp -CommandType Application 获取程序目录，Join-Path 定位同目录 README.md；通过 $env:WINDIR 定位 Web\Wallpaper\Windows\img0.jpg。只输出这些候选路径及是否存在，不读取配置、环境凭据或其他私密文件。
   - 对实际存在的 README 调用 filesystem_stat 和 filesystem_read_text(max_bytes=1024)。对实际存在的壁纸调用 image_read。不存在则标记未覆盖，不搜索个人文件。
   - 调用 computer_targets，再对已有目标调用 computer_state，省略 control_id，验证只读 actionable=false、state_id 为空。不要激活窗口或输入。
   - 再次调用 browser_status，记录选定实例连接状态及 last_seen。

6. 继续按最后成功结果的 output_cursor 采集自己 120 秒进程，直到 running=false。合并所有成功读取的增量输出，检查 tick 1..120 是否完整、是否只有 GPT-B 标记。记录首尾机器时间、最终 duration_ms、exit_code、stderr、timed_out、截断标志与实际采集时间。随后用最终游标做一次纯读，确认退出状态仍可读取且没有新输出；该结果不再拼接。duration_ms 是进程运行时长，不是你完成全部工具调用的耗时；较晚采集完成结果时也应约为 120 秒，调度可能造成少量偏差。

7. finally：成功取得控制权才用自己的 control_id release，并查询 status 确认清理结果；不释放任何其他 token。遇到异常时，对自己仍运行的测试进程使用 process_continue(terminate=true)，并记录清理结果。短进程若也返回 session_id，同样只清理自己的。

报告：版本；每项 PASS/FAIL/未覆盖及错误原文；每次浏览器 status 的 tick 时间、browser_id、connected、last_seen；进程首尾时间区间和输出完整性；桌面 acquire 时间区间、busy 时的占用 label、自己的释放结果。可以展示 process session_id、browser_id、tab_id，但不要展示 control_id。只摘要网页与图像元数据。
单边报告只证明你实际执行的项目；两份进程时间区间重叠且输出各自纯净才能证明进程并发；busy 同时指向另一任务标签才能支持桌面互斥；两个不同在线 browser_id 的标签与快照都成功才覆盖跨 Profile 路由。不能把未覆盖算成通过，也不能把正常 busy 算成故障。
```

## 合并判定

| 项目 | 两份报告需要提供的证据 |
| --- | --- |
| 空闲连接 | 已选实例在至少 90 秒的 Browser status-only 窗口内保持在线，`last_seen` 多次推进；扩展 Worker 调试窗口关闭，窗口内没有页面观察或输入。 |
| 进程并发 | A、B 的实际 tick 时间区间重叠，各自 120 条输出完整且标记纯净。 |
| 时长语义 | 完成结果的 `duration_ms` 对应约 120 秒进程生命周期；把采集时间与进程退出时间分开记录。 |
| Profile 路由 | A、B 固定选择不同在线实例，发现的标签属于对应 `browser_id`，快照返回对应 `tab_id`。 |
| 桌面互斥 | 一方取得控制权，另一方在其持有期间申请得到 busy，紧接着的 status 显示另一方任务标签；控制者最终只释放自己的 token。 |
| 其他只读能力 | 版本、文件元数据/文本、原生图片与桌面状态按各自返回结果判断。 |

空闲连接、路由和互斥均依赖测试前提及实际重叠窗口。保留 FAIL 和未覆盖项目可准确区分连接问题、资源竞争与测试条件。
