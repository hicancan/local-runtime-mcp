package jsrt

import "strings"

const helpIndex = `Local Runtime MCP (lrmcp) JS SDK — call help("<topic>") for details.
Topics: proc, fs, browser, tab, locator, desktop, img, caps, utils

Quick reference:
- proc:    proc.run(program, args?, opts?) | proc.run({ cmd, shell?, cwd?, env?, stdin?, timeout_seconds?, max_output_tokens?, yield_time_ms?, io_mode? }) | proc.continue(opts) | proc.kill(session_id)
- fs:      fs.list(path, opts?) | fs.stat(path, opts?) | fs.read(path, { start_line?, end_line?, line_count?, max_bytes?, with_sha256? }) | fs.write(path, content, opts?) | fs.patch(path, hunks, expected_sha256) | fs.search(path, query, opts?)
- browser: browser.status() | browser.connect(browser_id?) | browser.tabs(browser_idOrOpts?) | browser.open(url, opts?) | browser.tab(id) | browser.cdp(method, params?, browser_id?) | browser.traceStart(opts?) | browser.traceStop(file_path?, browser_id?) | browser.cleanup()
- tab:     tab.goto(url) | reload() | back() | forward() | close() | markHandoff() | snapshot(opts?) | screenshot(opts?) | eval(js) | cdp(method, params?) | getByRole/getByText/getByLabel/getByPlaceholder/locator | click/fill/press/scroll/handleDialog | network(filter?) | networkBody(req_id) | console(filter?) | emulate(opts) | css(selectorOrLoc) | heapSnapshot(file_path?) | logpoint(url, line, expr, col?) | screencastStart(opts?) | screencastStop()
- desktop: desktop.targets(opts?) | desktop.state(opts?) | desktop.wait(opts) | desktop.action(act) | desktop.control(kindOrOpts, opts?)
- img:     img.read(path, opts?) | emitImage(imgOrShotOrPath, label?)
- caps:    caps.list() | caps.call(name, args?, browser_id?, tab_id?)
- utils:   print(...args) | write(...args) | console.log(...) | sleep(ms) | help(topic?)

Tips:
- Top-level let/const/var/function/async function persist across turns in this session.
- A bare final expression (e.g. await browser.status()) is auto-returned in cell.value.
- Parallelize independent calls with await Promise.allSettled([...]).`

var helpTopics = map[string]string{
	"proc": `proc SDK (all methods return Promises):
- proc.run(program, args?, opts?)
    program: string (executable path or name, e.g. "git" or "D:\\Dev\\local-runtime-mcp\\lrmcp.exe")
    args?:   string[]
    opts?:   { cwd?, directory?, env?, environment?, stdin?, keep_stdin_open?, timeout_seconds?, max_output_bytes?, max_output_tokens?, yield_time_ms?, io_mode?: "pipe"|"pty", columns?, rows? }
- proc.run({ cmd, shell?, cwd?, env?, stdin?, keep_stdin_open?, timeout_seconds?, max_output_bytes?, max_output_tokens?, yield_time_ms?, io_mode?, columns?, rows? })
    cmd:     string (shell command line)
    shell?:  "pwsh" | "powershell" | "cmd" | "bash" | "sh" (default: pwsh on Windows, bash elsewhere)
  Returns: { session_id?, program, cwd, exit_code, running, timed_out, duration_ms, stdout, stderr, stdout_truncated, stderr_truncated, output_cursor }
- proc.continue({ session_id, output_cursor?, stdin?, close_stdin?, terminate?, yield_time_ms?, columns?, rows? })
- proc.kill(session_id)

Example:
  const [v, t] = await Promise.all([proc.run("git", ["status", "-sb"]), proc.run({ cmd: "Get-Date" })]);`,

	"fs": `fs SDK (all methods return Promises; options use strict snake_case):
- fs.list(path, { max_depth?, include_hidden?, max_entries? }?)
  Returns: { entries: [{ path, type: "file"|"directory", size?, modified_at }], truncated, skipped }
- fs.stat(path, { with_sha256? }? | bool?)
  Returns: { path, type, size, modified_at, mime_type?, symlink_target?, sha256? }
- fs.read(path, { start_line?, end_line?, line_count?, max_bytes?, with_sha256? }?)
  Note: 1-based inclusive line numbers; pass end_line OR line_count (not both).
  Returns: { path, content, size, start_line, end_line, next_line?, truncated, sha256? }
- fs.write(path, content, { mode?: "create"|"replace", expected_sha256?, create_parents? }?)
  Returns: { path, bytes, created, sha256 }
- fs.patch(path, hunks, expected_sha256)
    hunks: [{ before?, old, new, after? }]
  Returns: { path, bytes, hunks, sha256 }
- fs.search(path, query, { regex?, case_sensitive?, include_hidden?, max_results? }?)
  Returns: { matches: [{ path, line, column, text }], truncated, skipped }

Example:
  const cfg = await fs.read("D:\\Dev\\local-runtime-mcp\\config.example.yaml", { start_line: 1, end_line: 25 });`,

	"browser": `browser SDK (native browser-level CDP; all methods except browser.tab return Promises):
- browser.status()
  Returns: { configured, connected, address, extension_connected, instances: [{ browser_id, label, browser, state, cdp_url, connected, extension_version, last_seen }] }
- browser.connect(browser_id?)
  Connects to Edge/Chrome DevToolsActivePort (browser_id e.g. "edge" or "chrome").
- browser.tabs(browser_id? | { browser_id?, include_workers? }?)
  Returns: TabHandle[] (each has .id, .target_id, .browser_id, .type, .title, .url, .active, .handed_off + tab methods)
- browser.open(url, { browser_id?, active?, control_id?, session_name?, group? }?)
  Returns: TabHandle (temporary tab auto-reclaimed on session(done) unless tab.markHandoff() is called)
- browser.tab(tab_id) -> synchronous TabHandle wrapper for a known tab ID
- browser.cdp(method, params?, browser_id?) -> raw browser-root CDP command
- browser.traceStart({ browser_id?, tab_id?, reload?, ignore_cache?, categories? }?) -> browser-process-level Tracing.start
- browser.traceStop(file_path? | { file_path?, browser_id? }?, browser_id?) -> { file_path, bytes, duration_ms, event_count, web_vitals, long_tasks }
- browser.cleanup() -> closes all non-handed-off tabs opened in this session`,

	"tab": `TabHandle methods (returned by browser.tabs() / browser.open() / browser.tab(id)):
Properties: id, target_id, browser_id, type, title, url, active, handed_off
Navigation & lifecycle:
- tab.goto(url) | tab.reload() | tab.back() | tab.forward() | tab.close()
- tab.markHandoff() (sync; keeps tab open for the user after session(done))
Inspection & scripting:
- tab.snapshot({ diff?: true, max_elements?: 500, max_text?: 50000 }?) -> incremental AX tree + refs
- tab.screenshot({ full_page?, clip_x?, clip_y?, clip_width?, clip_height? }?) -> pass result to emitImage(shot, label)
- tab.eval(script) -> evaluates JS in page context and returns value
- tab.cdp(method, params?) -> raw page-target CDP command
Locators (sync handle -> async actions with auto-wait):
- tab.locator(css | { role?, name?, exact?, text?, label?, placeholder?, css?, ref?, nth?, timeout_ms? })
- tab.getByRole(role, name? | { name?, exact?, nth?, timeout_ms? }?)
- tab.getByText(text, { exact?, nth?, timeout_ms? }?)
- tab.getByLabel(label, { exact?, nth?, timeout_ms? }?)
- tab.getByPlaceholder(placeholder, { exact?, nth?, timeout_ms? }?)
Direct actions:
- tab.click(ref | actionOpts) | tab.fill(ref, text) | tab.press(key, ref?) | tab.scroll(dx, dy) | tab.handleDialog(accept?, prompt_text?) | tab.action(actionOpts)
DevTools:
- tab.network({ resource_types?, url_contains?, include_preserved?, failed_only?, limit? }?)
- tab.networkBody(request_id) -> { request_id, body, base64_encoded? }
- tab.console({ levels?, limit? }?)
- tab.emulate({ width?, height?, device_scale_factor?, mobile?, touch?, landscape?, clear_viewport?, cpu_throttling_rate?, network_preset?, color_scheme?, reduced_motion?, forced_colors?, user_agent?, geolocation?, extra_headers? })
- tab.css(cssSelector | locatorObj) -> { selector, computed, matched_rules }
- tab.heapSnapshot(file_path?) | tab.logpoint(url, line, expr, col?) | tab.screencastStart({ max_width?, max_height?, quality? }?) | tab.screencastStop()`,

	"locator": `LocatorHandle methods (returned by tab.locator / getByRole / getByText / getByLabel / getByPlaceholder):
- loc.click() | loc.doubleClick() | loc.hover()
- loc.fill(text) | loc.type(text) | loc.press(key) | loc.select(option) | loc.check(checked?)
- loc.uploadFiles(pathsArray)
- loc.waitFor({ timeout_ms? }?)
- loc.nth(index) -> returns narrowed LocatorHandle`,

	"desktop": `desktop SDK (native Windows Rust WGC + UI Automation):
- desktop.targets({ pid?, title?, process? }?) -> { displays, windows }
- desktop.state({ control_id?, target_id?, display_id?, region?, image?, cursor?, elements?, name?, role?, automation_id?, max_elements?, max_width?, max_height? }?) -> pass to emitImage()
- desktop.wait({ target_id?, display_id?, name?, role?, automation_id?, timeout_seconds?, image? })
- desktop.action({ kind, control_id?, target_id?, element_ref?, x?, y?, to_x?, to_y?, button?, text?, key?, keys?, scroll_x?, scroll_y? })
- desktop.control("status" | "acquire" | "release", { control_id?, label? }?)`,

	"img": `img & emitImage:
- img.read(path, { max_bytes?, max_pixels?, crop_x?, crop_y?, crop_width?, crop_height?, max_width?, max_height? }?)
- emitImage(imgOrShotOrPath, label?) -> attaches native MCP ImageContent to this cell's result`,

	"caps": `caps SDK (owner-approved site/app capabilities):
- caps.list() -> Promise<CapabilityDefinition[]>
- caps.call(name, args?, browser_id?, tab_id?) -> Promise<any>`,

	"utils": `Built-in helpers:
- print(...args) / console.log(...args) -> appends line to cell.output
- write(...args) -> appends raw text without trailing newline
- sleep(ms) -> Promise<true> (capped at 300000ms)
- help(topic?) -> returns SDK documentation string`,
}

func lookupHelp(topic string) string {
	t := strings.ToLower(strings.TrimSpace(topic))
	if t == "" {
		return helpIndex
	}
	if doc, ok := helpTopics[t]; ok {
		return doc
	}
	// Map sub-topics like "fs.read", "proc.run", "browser.open", "tab.snapshot"
	if dot := strings.IndexByte(t, '.'); dot > 0 {
		prefix := t[:dot]
		if doc, ok := helpTopics[prefix]; ok {
			return doc
		}
	}
	if t == "emitimage" || t == "image" {
		return helpTopics["img"]
	}
	if t == "print" || t == "sleep" || t == "write" || t == "console" || t == "help" {
		return helpTopics["utils"]
	}
	return "Unknown help topic " + topic + ".\n\n" + helpIndex
}
