package jsrt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	runtimeimage "github.com/hicancan/local-runtime-mcp/internal/image"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
)

func (v *sessionVM) installGlobals() {
	_ = v.vm.Set("proc", v.buildProcSDK())
	_ = v.vm.Set("fs", v.buildFSSDK())
	_ = v.vm.Set("browser", v.buildBrowserSDK())
	_ = v.vm.Set("desktop", v.buildDesktopSDK())
	_ = v.vm.Set("img", v.buildImageSDK())
	_ = v.vm.Set("caps", v.buildCapsSDK())

	printFn := v.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if cell := v.currentCell(); cell != nil {
			parts := make([]string, 0, len(call.Arguments))
			for _, arg := range call.Arguments {
				parts = append(parts, formatJSArg(arg))
			}
			cell.appendOutput(strings.Join(parts, " ") + "\n")
		}
		return goja.Undefined()
	})
	writeFn := v.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if cell := v.currentCell(); cell != nil {
			for _, arg := range call.Arguments {
				cell.appendOutput(formatJSArg(arg))
			}
		}
		return goja.Undefined()
	})
	consoleObj := v.vm.NewObject()
	_ = consoleObj.Set("log", printFn)
	_ = consoleObj.Set("info", printFn)
	_ = consoleObj.Set("warn", printFn)
	_ = consoleObj.Set("error", printFn)

	emitImageFn := v.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		cell := v.currentCell()
		if cell == nil {
			panic(v.vm.NewTypeError("no active execution cell"))
		}
		if len(call.Arguments) == 0 {
			panic(v.vm.NewTypeError("emitImage requires an image result or file path"))
		}
		label := ""
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			label = call.Arguments[1].String()
		}
		arg := call.Arguments[0]
		if s, ok := arg.Export().(string); ok && s != "" {
			data, meta, err := runtimeimage.Read(cell.ctx, s, runtimeimage.ReadOptions{})
			if err != nil {
				panic(v.vm.NewGoError(err))
			}
			cell.addImage(ImagePayload{Data: data, MIMEType: meta.MIMEType, Label: label, Width: meta.Width, Height: meta.Height})
			return v.vm.ToValue(map[string]any{"emitted": true, "mime_type": meta.MIMEType, "width": meta.Width, "height": meta.Height, "bytes": len(data)})
		}
		if obj := arg.ToObject(v.vm); obj != nil {
			if raw := obj.Get("__image_bytes"); raw != nil && !goja.IsUndefined(raw) {
				if data, ok := raw.Export().([]byte); ok && len(data) > 0 {
					mime := "image/png"
					if m := obj.Get("mime_type"); m != nil && !goja.IsUndefined(m) {
						mime = m.String()
					}
					w, h := 0, 0
					if wv := obj.Get("width"); wv != nil && !goja.IsUndefined(wv) {
						w = int(wv.ToInteger())
					}
					if hv := obj.Get("height"); hv != nil && !goja.IsUndefined(hv) {
						h = int(hv.ToInteger())
					}
					cell.addImage(ImagePayload{Data: data, MIMEType: mime, Label: label, Width: w, Height: h})
					return v.vm.ToValue(map[string]any{"emitted": true, "mime_type": mime, "width": w, "height": h, "bytes": len(data)})
				}
			}
		}
		panic(v.vm.NewTypeError("emitImage expects an image path or a result from img.read / tab.screenshot / desktop.state"))
	})

	sleepFn := v.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		ms := int64(0)
		if len(call.Arguments) > 0 {
			ms = call.Arguments[0].ToInteger()
		}
		if ms < 0 {
			ms = 0
		}
		if ms > 300_000 {
			ms = 300_000
		}
		return v.spawnAsync("sleep", fmt.Sprintf("%dms", ms), func(ctx context.Context) (any, error) {
			t := time.NewTimer(time.Duration(ms) * time.Millisecond)
			defer t.Stop()
			select {
			case <-t.C:
				return true, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	})

	helpFn := v.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		topic := ""
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			topic = call.Arguments[0].String()
		}
		return v.vm.ToValue(lookupHelp(topic))
	})

	_ = v.vm.Set("print", printFn)
	_ = v.vm.Set("write", writeFn)
	_ = v.vm.Set("console", consoleObj)
	_ = v.vm.Set("emitImage", emitImageFn)
	_ = v.vm.Set("sleep", sleepFn)
	_ = v.vm.Set("help", helpFn)
}

func (v *sessionVM) isReadOnly() bool {
	cell := v.currentCell()
	return cell != nil && cell.readOnly
}

func (v *sessionVM) currentSessionID() string {
	if cell := v.currentCell(); cell != nil {
		return cell.sessionID
	}
	return v.sessionID
}

func (v *sessionVM) buildProcSDK() goja.Value {
	obj := v.vm.NewObject()
	_ = obj.Set("run", func(call goja.FunctionCall) goja.Value {
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids process execution"))
		}
		if v.deps.Process == nil {
			return v.rejected(errors.New("process manager unavailable"))
		}
		opts, target, err := parseProcRunArgs(call)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("proc.run", target, func(ctx context.Context) (any, error) {
			res, err := v.deps.Process.Run(ctx, opts)
			if err != nil {
				return nil, err
			}
			return toPlain(res), nil
		})
	})

	_ = obj.Set("continue", func(call goja.FunctionCall) goja.Value {
		const sig = `proc.continue({ session_id, output_cursor?, stdin?, close_stdin?, terminate?, yield_time_ms?, columns?, rows? })`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids process control"))
		}
		if v.deps.Process == nil {
			return v.rejected(errors.New("process manager unavailable"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		if len(call.Arguments) == 0 {
			return v.rejected(errors.New("proc.continue requires options object; signature: " + sig))
		}
		var opts runtimeprocess.ContinueOptions
		if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
			return v.rejected(err)
		}
		if strings.TrimSpace(opts.SessionID) == "" {
			return v.rejected(errors.New("proc.continue requires session_id; signature: " + sig))
		}
		return v.spawnAsync("proc.continue", opts.SessionID, func(ctx context.Context) (any, error) {
			res, err := v.deps.Process.Continue(ctx, opts)
			if err != nil {
				return nil, err
			}
			return toPlain(res), nil
		})
	})

	_ = obj.Set("kill", func(call goja.FunctionCall) goja.Value {
		const sig = `proc.kill(session_id)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids process control"))
		}
		if v.deps.Process == nil {
			return v.rejected(errors.New("process manager unavailable"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		sid, err := requireNonEmptyStringArg(call, 0, "session_id", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("proc.kill", sid, func(ctx context.Context) (any, error) {
			res, err := v.deps.Process.Continue(ctx, runtimeprocess.ContinueOptions{SessionID: sid, Terminate: true, YieldTimeMS: 2000})
			if err != nil {
				return nil, err
			}
			return toPlain(res), nil
		})
	})
	return obj
}

func parseProcRunArgs(call goja.FunctionCall) (runtimeprocess.Options, string, error) {
	const sig = `proc.run(program, args?, { cwd?, env?, stdin?, keep_stdin_open?, timeout_seconds?, max_output_bytes?, max_output_tokens?, yield_time_ms?, io_mode?, columns?, rows? }) or proc.run({ cmd, shell?, cwd?, env?, ... })`
	if len(call.Arguments) == 0 {
		return runtimeprocess.Options{}, "", errors.New("proc.run requires a program string or options object; signature: " + sig)
	}
	var opts runtimeprocess.Options
	var cmdStr, shellName string
	maxTokens := 0

	if prog, ok := call.Arguments[0].Export().(string); ok {
		if err := rejectExtraArgs(call, 3, sig); err != nil {
			return runtimeprocess.Options{}, "", err
		}
		opts.Program = prog
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			if err := decodeStrictSlice(call.Arguments[1], &opts.Args, "args", sig); err != nil {
				return runtimeprocess.Options{}, "", err
			}
		}
		if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
			var extra struct {
				Cwd             string            `json:"cwd"`
				Workdir         string            `json:"workdir"`
				Directory       string            `json:"directory"`
				Environment     map[string]string `json:"environment"`
				Env             map[string]string `json:"env"`
				Stdin           string            `json:"stdin"`
				KeepStdinOpen   bool              `json:"keep_stdin_open"`
				TimeoutSeconds  int               `json:"timeout_seconds"`
				MaxOutputBytes  int               `json:"max_output_bytes"`
				MaxOutputTokens int               `json:"max_output_tokens"`
				YieldTimeMS     int               `json:"yield_time_ms"`
				IOMode          string            `json:"io_mode"`
				Columns         int               `json:"columns"`
				Rows            int               `json:"rows"`
			}
			if err := decodeStrictObj(call.Arguments[2], &extra, sig); err != nil {
				return runtimeprocess.Options{}, "", err
			}
			opts.Directory = firstNonEmpty(extra.Directory, extra.Cwd, extra.Workdir)
			if len(extra.Environment) > 0 {
				opts.Environment = extra.Environment
			} else if len(extra.Env) > 0 {
				opts.Environment = extra.Env
			}
			opts.Stdin = extra.Stdin
			opts.KeepStdinOpen = extra.KeepStdinOpen
			opts.TimeoutSeconds = extra.TimeoutSeconds
			opts.MaxOutputBytes = extra.MaxOutputBytes
			maxTokens = extra.MaxOutputTokens
			opts.YieldTimeMS = extra.YieldTimeMS
			opts.IOMode = extra.IOMode
			opts.Columns = extra.Columns
			opts.Rows = extra.Rows
		}
	} else {
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return runtimeprocess.Options{}, "", err
		}
		var raw struct {
			Program         string            `json:"program"`
			Args            []string          `json:"args"`
			Cmd             string            `json:"cmd"`
			Shell           string            `json:"shell"`
			Cwd             string            `json:"cwd"`
			Workdir         string            `json:"workdir"`
			Directory       string            `json:"directory"`
			Environment     map[string]string `json:"environment"`
			Env             map[string]string `json:"env"`
			Stdin           string            `json:"stdin"`
			KeepStdinOpen   bool              `json:"keep_stdin_open"`
			TimeoutSeconds  int               `json:"timeout_seconds"`
			MaxOutputBytes  int               `json:"max_output_bytes"`
			MaxOutputTokens int               `json:"max_output_tokens"`
			YieldTimeMS     int               `json:"yield_time_ms"`
			IOMode          string            `json:"io_mode"`
			Columns         int               `json:"columns"`
			Rows            int               `json:"rows"`
		}
		if err := decodeStrictObj(call.Arguments[0], &raw, sig); err != nil {
			return runtimeprocess.Options{}, "", err
		}
		opts.Program = raw.Program
		opts.Args = raw.Args
		cmdStr = raw.Cmd
		shellName = raw.Shell
		opts.Directory = firstNonEmpty(raw.Directory, raw.Cwd, raw.Workdir)
		if len(raw.Environment) > 0 {
			opts.Environment = raw.Environment
		} else if len(raw.Env) > 0 {
			opts.Environment = raw.Env
		}
		opts.Stdin = raw.Stdin
		opts.KeepStdinOpen = raw.KeepStdinOpen
		opts.TimeoutSeconds = raw.TimeoutSeconds
		opts.MaxOutputBytes = raw.MaxOutputBytes
		maxTokens = raw.MaxOutputTokens
		opts.YieldTimeMS = raw.YieldTimeMS
		opts.IOMode = raw.IOMode
		opts.Columns = raw.Columns
		opts.Rows = raw.Rows
	}

	if opts.Program == "" && cmdStr != "" {
		sh := strings.ToLower(strings.TrimSpace(shellName))
		if sh == "" {
			if runtime.GOOS == "windows" {
				sh = "pwsh"
			} else {
				sh = "bash"
			}
		}
		switch sh {
		case "pwsh", "powershell":
			opts.Program = "pwsh.exe"
			if runtime.GOOS != "windows" {
				opts.Program = "pwsh"
			}
			opts.Args = []string{"-NoProfile", "-NonInteractive", "-Command", cmdStr}
		case "cmd":
			opts.Program = "cmd.exe"
			opts.Args = []string{"/C", cmdStr}
		case "sh":
			opts.Program = "sh"
			opts.Args = []string{"-c", cmdStr}
		default:
			opts.Program = "bash"
			opts.Args = []string{"-lc", cmdStr}
		}
	}
	if opts.Program == "" {
		return runtimeprocess.Options{}, "", errors.New("proc.run requires program or cmd")
	}
	if opts.MaxOutputBytes == 0 && maxTokens > 0 {
		opts.MaxOutputBytes = max(1024, min(16<<20, maxTokens*4))
	}
	target := opts.Program
	if cmdStr != "" {
		target = clipStr(cmdStr, 80)
	} else if len(opts.Args) > 0 {
		target = clipStr(opts.Program+" "+strings.Join(opts.Args, " "), 80)
	}
	return opts, target, nil
}

func (v *sessionVM) buildFSSDK() goja.Value {
	obj := v.vm.NewObject()
	_ = obj.Set("list", func(call goja.FunctionCall) goja.Value {
		const sig = `fs.list(path, { max_depth?, include_hidden?, max_entries? }?)`
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		path, err := requireNonEmptyStringArg(call, 0, "path", sig)
		if err != nil {
			return v.rejected(err)
		}
		var opts struct {
			MaxDepth      int  `json:"max_depth"`
			IncludeHidden bool `json:"include_hidden"`
			MaxEntries    int  `json:"max_entries"`
		}
		if len(call.Arguments) > 1 {
			if err := decodeStrictObj(call.Arguments[1], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("fs.list", path, func(_ context.Context) (any, error) {
			res, err := filesystem.List(path, opts.MaxDepth, opts.IncludeHidden, opts.MaxEntries)
			return toPlain(res), err
		})
	})
	_ = obj.Set("stat", func(call goja.FunctionCall) goja.Value {
		const sig = `fs.stat(path, { with_sha256? }? | bool?)`
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		path, err := requireNonEmptyStringArg(call, 0, "path", sig)
		if err != nil {
			return v.rejected(err)
		}
		withSHA := false
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			if b, ok := call.Arguments[1].Export().(bool); ok {
				withSHA = b
			} else {
				var opts struct {
					WithSHA256 bool `json:"with_sha256"`
				}
				if err := decodeStrictObj(call.Arguments[1], &opts, sig); err != nil {
					return v.rejected(err)
				}
				withSHA = opts.WithSHA256
			}
		}
		return v.spawnAsync("fs.stat", path, func(_ context.Context) (any, error) {
			res, err := filesystem.Stat(path, withSHA)
			return toPlain(res), err
		})
	})
	_ = obj.Set("read", func(call goja.FunctionCall) goja.Value {
		const sig = `fs.read(path, { start_line?, end_line?, line_count?, max_bytes?, with_sha256? }?)`
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejectedOp("fs.read", "", err)
		}
		path, err := requireNonEmptyStringArg(call, 0, "path", sig)
		if err != nil {
			return v.rejectedOp("fs.read", "", err)
		}
		var raw struct {
			StartLine  int  `json:"start_line"`
			EndLine    int  `json:"end_line"`
			LineCount  int  `json:"line_count"`
			MaxBytes   int  `json:"max_bytes"`
			WithSHA256 bool `json:"with_sha256"`
		}
		if len(call.Arguments) > 1 {
			if err := decodeStrictObj(call.Arguments[1], &raw, sig); err != nil {
				return v.rejectedOp("fs.read", path, err)
			}
		}
		if raw.StartLine < 0 || raw.EndLine < 0 || raw.LineCount < 0 {
			return v.rejectedOp("fs.read", path, errors.New("start_line, end_line, and line_count must be >= 0; signature: "+sig))
		}
		if raw.EndLine > 0 && raw.LineCount > 0 {
			return v.rejectedOp("fs.read", path, errors.New("pass either end_line or line_count, not both; signature: "+sig))
		}
		opts := filesystem.ReadTextOptions{
			StartLine:  raw.StartLine,
			LineCount:  raw.LineCount,
			MaxBytes:   raw.MaxBytes,
			WithSHA256: raw.WithSHA256,
		}
		if raw.EndLine > 0 {
			start := raw.StartLine
			if start == 0 {
				start = 1
			}
			if raw.EndLine < start {
				return v.rejectedOp("fs.read", path, fmt.Errorf("end_line (%d) must be >= start_line (%d); signature: %s", raw.EndLine, start, sig))
			}
			opts.StartLine = start
			opts.LineCount = raw.EndLine - start + 1
		}
		return v.spawnAsync("fs.read", path, func(_ context.Context) (any, error) {
			res, err := filesystem.ReadText(path, opts)
			return toPlain(res), err
		})
	})
	_ = obj.Set("write", func(call goja.FunctionCall) goja.Value {
		const sig = `fs.write(path, content, { mode?: "create"|"replace", expected_sha256?, create_parents? }?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids fs.write"))
		}
		if v.deps.Filesystem == nil {
			return v.rejected(errors.New("filesystem service unavailable"))
		}
		if err := rejectExtraArgs(call, 3, sig); err != nil {
			return v.rejected(err)
		}
		path, err := requireNonEmptyStringArg(call, 0, "path", sig)
		if err != nil {
			return v.rejected(err)
		}
		content, err := requireStringArg(call, 1, "content", sig)
		if err != nil {
			return v.rejected(err)
		}
		opts := filesystem.WriteTextOptions{Mode: "create"}
		if len(call.Arguments) > 2 {
			if err := decodeStrictObj(call.Arguments[2], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("fs.write", path, func(ctx context.Context) (any, error) {
			res, err := v.deps.Filesystem.WriteText(ctx, path, content, opts)
			return toPlain(res), err
		})
	})
	_ = obj.Set("patch", func(call goja.FunctionCall) goja.Value {
		const sig = `fs.patch(path, [{ before?, old, new, after? }], expected_sha256)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids fs.patch"))
		}
		if v.deps.Filesystem == nil {
			return v.rejected(errors.New("filesystem service unavailable"))
		}
		if err := rejectExtraArgs(call, 3, sig); err != nil {
			return v.rejected(err)
		}
		path, err := requireNonEmptyStringArg(call, 0, "path", sig)
		if err != nil {
			return v.rejected(err)
		}
		if len(call.Arguments) < 2 {
			return v.rejected(errors.New("fs.patch requires hunks; signature: " + sig))
		}
		var hunks []filesystem.TextPatchHunk
		if err := decodeStrictSlice(call.Arguments[1], &hunks, "hunks", sig); err != nil {
			return v.rejected(err)
		}
		sha, err := requireNonEmptyStringArg(call, 2, "expected_sha256", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("fs.patch", path, func(ctx context.Context) (any, error) {
			res, err := v.deps.Filesystem.PatchText(ctx, path, filesystem.PatchTextOptions{Hunks: hunks, ExpectedSHA256: sha})
			return toPlain(res), err
		})
	})
	_ = obj.Set("search", func(call goja.FunctionCall) goja.Value {
		const sig = `fs.search(path, query, { regex?, case_sensitive?, include_hidden?, max_results? }?)`
		if err := rejectExtraArgs(call, 3, sig); err != nil {
			return v.rejected(err)
		}
		path, err := requireNonEmptyStringArg(call, 0, "path", sig)
		if err != nil {
			return v.rejected(err)
		}
		query, err := requireNonEmptyStringArg(call, 1, "query", sig)
		if err != nil {
			return v.rejected(err)
		}
		var opts struct {
			Regex         bool `json:"regex"`
			CaseSensitive bool `json:"case_sensitive"`
			IncludeHidden bool `json:"include_hidden"`
			MaxResults    int  `json:"max_results"`
		}
		if len(call.Arguments) > 2 {
			if err := decodeStrictObj(call.Arguments[2], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("fs.search", query+" in "+path, func(_ context.Context) (any, error) {
			res, err := filesystem.SearchText(path, query, opts.Regex, opts.CaseSensitive, opts.IncludeHidden, opts.MaxResults)
			return toPlain(res), err
		})
	})
	return obj
}

func (v *sessionVM) buildBrowserSDK() goja.Value {
	obj := v.vm.NewObject()
	b := v.deps.Browser

	_ = obj.Set("status", func(call goja.FunctionCall) goja.Value {
		if b == nil {
			return v.rejected(errors.New("browser engine unavailable"))
		}
		return v.spawnAsync("browser.status", "", func(_ context.Context) (any, error) {
			return toPlain(b.Status()), nil
		})
	})
	_ = obj.Set("connect", func(call goja.FunctionCall) goja.Value {
		const sig = `browser.connect(browser_id?)`
		if b == nil {
			return v.rejected(errors.New("browser engine unavailable"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		browserID, err := optionalStringArg(call, 0, "browser_id", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("browser.connect", browserID, func(ctx context.Context) (any, error) {
			inst, err := b.Connect(ctx, browserID)
			return toPlain(inst), err
		})
	})
	_ = obj.Set("tabs", func(call goja.FunctionCall) goja.Value {
		const sig = `browser.tabs(browser_id? | { browser_id?, include_workers? }?, { include_workers? }?)`
		if b == nil {
			return v.rejected(errors.New("browser engine unavailable"))
		}
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		browserID := ""
		includeWorkers := false
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			if s, ok := call.Arguments[0].Export().(string); ok {
				browserID = s
			} else {
				if err := rejectExtraArgs(call, 1, sig); err != nil {
					return v.rejected(err)
				}
				var opts struct {
					BrowserID      string `json:"browser_id"`
					IncludeWorkers bool   `json:"include_workers"`
				}
				if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
					return v.rejected(err)
				}
				browserID = opts.BrowserID
				includeWorkers = opts.IncludeWorkers
			}
		}
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			var opts struct {
				IncludeWorkers bool `json:"include_workers"`
			}
			if err := decodeStrictObj(call.Arguments[1], &opts, sig); err != nil {
				return v.rejected(err)
			}
			includeWorkers = opts.IncludeWorkers
		}
		return v.spawnAsync("browser.tabs", browserID, func(ctx context.Context) (any, error) {
			tabs, err := b.TabsWithOptions(ctx, browserID, includeWorkers)
			if err != nil {
				return nil, err
			}
			return tabs, nil
		}, func(raw any) goja.Value {
			tabs := raw.([]browser.Tab)
			arr := make([]goja.Value, len(tabs))
			for i, t := range tabs {
				arr[i] = v.makeTabHandle(t)
			}
			return v.vm.ToValue(arr)
		})
	})
	_ = obj.Set("open", func(call goja.FunctionCall) goja.Value {
		const sig = `browser.open(url, { browser_id?, active?, control_id?, session_name?, group? }?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids browser.open"))
		}
		if b == nil {
			return v.rejected(errors.New("browser engine unavailable"))
		}
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		urlStr, err := requireNonEmptyStringArg(call, 0, "url", sig)
		if err != nil {
			return v.rejected(err)
		}
		var opts struct {
			BrowserID   string `json:"browser_id"`
			Active      bool   `json:"active"`
			ControlID   string `json:"control_id"`
			SessionName string `json:"session_name"`
			Group       string `json:"group"`
		}
		if len(call.Arguments) > 1 {
			if err := decodeStrictObj(call.Arguments[1], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		groupName := firstNonEmpty(opts.SessionName, opts.Group)
		sid := v.currentSessionID()
		return v.spawnAsync("browser.open", clipStr(urlStr, 90), func(ctx context.Context) (any, error) {
			return b.OpenWithOptions(ctx, browser.OpenOptions{
				BrowserID:   opts.BrowserID,
				URL:         urlStr,
				Active:      opts.Active,
				ControlID:   opts.ControlID,
				SessionID:   sid,
				SessionName: groupName,
			})
		}, func(raw any) goja.Value {
			return v.makeTabHandle(raw.(browser.Tab))
		})
	})
	_ = obj.Set("tab", func(call goja.FunctionCall) goja.Value {
		const sig = `browser.tab(tab_id)`
		id, err := requireNonEmptyStringArg(call, 0, "tab_id", sig)
		if err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		return v.makeTabHandle(browser.Tab{ID: id})
	})
	_ = obj.Set("cdp", func(call goja.FunctionCall) goja.Value {
		const sig = `browser.cdp(method, params?, browser_id?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids raw browser.cdp"))
		}
		if b == nil {
			return v.rejected(errors.New("browser engine unavailable"))
		}
		if err := rejectExtraArgs(call, 3, sig); err != nil {
			return v.rejected(err)
		}
		method, err := requireNonEmptyStringArg(call, 0, "method", sig)
		if err != nil {
			return v.rejected(err)
		}
		var params map[string]any
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			if err := decodeStrictObj(call.Arguments[1], &params, sig); err != nil {
				return v.rejected(err)
			}
		}
		browserID, err := optionalStringArg(call, 2, "browser_id", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("browser.cdp", method, func(ctx context.Context) (any, error) {
			return b.RawCDP(ctx, browserID, "", method, params)
		})
	})
	_ = obj.Set("traceStart", func(call goja.FunctionCall) goja.Value {
		const sig = `browser.traceStart({ browser_id?, tab_id?, reload?, ignore_cache?, categories? }?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids browser.traceStart"))
		}
		if b == nil {
			return v.rejected(errors.New("browser engine unavailable"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var opts browser.TraceStartOptions
		if len(call.Arguments) > 0 {
			if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("browser.traceStart", opts.BrowserID, func(ctx context.Context) (any, error) {
			return map[string]any{"tracing": true}, b.TraceStart(ctx, opts)
		})
	})
	_ = obj.Set("traceStop", func(call goja.FunctionCall) goja.Value {
		const sig = `browser.traceStop(file_path? | { file_path?, browser_id? }?, browser_id?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids browser.traceStop"))
		}
		if b == nil {
			return v.rejected(errors.New("browser engine unavailable"))
		}
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		filePath := ""
		browserID := ""
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			if s, ok := call.Arguments[0].Export().(string); ok {
				filePath = s
			} else {
				if err := rejectExtraArgs(call, 1, sig); err != nil {
					return v.rejected(err)
				}
				var opts struct {
					FilePath  string `json:"file_path"`
					BrowserID string `json:"browser_id"`
				}
				if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
					return v.rejected(err)
				}
				filePath = opts.FilePath
				browserID = opts.BrowserID
			}
		}
		if len(call.Arguments) > 1 {
			bid, err := optionalStringArg(call, 1, "browser_id", sig)
			if err != nil {
				return v.rejected(err)
			}
			if bid != "" {
				browserID = bid
			}
		}
		return v.spawnAsync("browser.traceStop", filePath, func(ctx context.Context) (any, error) {
			res, err := b.TraceStop(ctx, browserID, filePath)
			return toPlain(res), err
		})
	})
	_ = obj.Set("cleanup", func(call goja.FunctionCall) goja.Value {
		if v.isReadOnly() || b == nil {
			return v.rejected(errors.New("cleanup unavailable"))
		}
		sid := v.currentSessionID()
		return v.spawnAsync("browser.cleanup", sid, func(ctx context.Context) (any, error) {
			closed := b.CleanupSessionTabs(ctx, sid)
			return map[string]any{"closed": closed}, nil
		})
	})
	return obj
}

func (v *sessionVM) makeTabHandle(tab browser.Tab) goja.Value {
	b := v.deps.Browser
	obj := v.vm.NewObject()
	_ = obj.Set("id", tab.ID)
	_ = obj.Set("target_id", tab.TargetID)
	_ = obj.Set("browser_id", tab.BrowserID)
	_ = obj.Set("type", tab.Type)
	_ = obj.Set("title", tab.Title)
	_ = obj.Set("url", tab.URL)
	_ = obj.Set("active", tab.Active)
	_ = obj.Set("handed_off", tab.HandedOff)

	tabID := tab.ID
	label := firstNonEmpty(tab.Title, tab.URL, tabID)

	_ = obj.Set("goto", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.goto(url)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.goto"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		u, err := requireNonEmptyStringArg(call, 0, "url", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("tab.goto", clipStr(u, 90), func(ctx context.Context) (any, error) {
			updated, err := b.Navigate(ctx, browser.Navigation{TabID: tabID, Kind: "url", URL: u})
			return toPlain(updated), err
		})
	})
	_ = obj.Set("reload", func(call goja.FunctionCall) goja.Value {
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.reload"))
		}
		return v.spawnAsync("tab.reload", label, func(ctx context.Context) (any, error) {
			updated, err := b.Navigate(ctx, browser.Navigation{TabID: tabID, Kind: "reload"})
			return toPlain(updated), err
		})
	})
	_ = obj.Set("back", func(call goja.FunctionCall) goja.Value {
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.back"))
		}
		return v.spawnAsync("tab.back", label, func(ctx context.Context) (any, error) {
			updated, err := b.Navigate(ctx, browser.Navigation{TabID: tabID, Kind: "back"})
			return toPlain(updated), err
		})
	})
	_ = obj.Set("forward", func(call goja.FunctionCall) goja.Value {
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.forward"))
		}
		return v.spawnAsync("tab.forward", label, func(ctx context.Context) (any, error) {
			updated, err := b.Navigate(ctx, browser.Navigation{TabID: tabID, Kind: "forward"})
			return toPlain(updated), err
		})
	})
	_ = obj.Set("close", func(call goja.FunctionCall) goja.Value {
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.close"))
		}
		return v.spawnAsync("tab.close", label, func(ctx context.Context) (any, error) {
			return map[string]any{"closed": true}, b.CloseTab(ctx, tabID, "")
		})
	})
	_ = obj.Set("markHandoff", func(call goja.FunctionCall) goja.Value {
		if err := b.MarkHandoff(tabID); err != nil {
			panic(v.vm.NewGoError(err))
		}
		_ = obj.Set("handed_off", true)
		return v.vm.ToValue(true)
	})
	_ = obj.Set("snapshot", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.snapshot({ diff?, max_elements?, max_text? }?)`
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		opts := browser.SnapshotOptions{MaxElements: 500, MaxText: 50000, Diff: true}
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			var raw struct {
				MaxElements int   `json:"max_elements"`
				MaxText     int   `json:"max_text"`
				Diff        *bool `json:"diff"`
			}
			if err := decodeStrictObj(call.Arguments[0], &raw, sig); err != nil {
				return v.rejected(err)
			}
			if raw.MaxElements > 0 {
				opts.MaxElements = raw.MaxElements
			}
			if raw.MaxText > 0 {
				opts.MaxText = raw.MaxText
			}
			if raw.Diff != nil {
				opts.Diff = *raw.Diff
			}
		}
		return v.spawnAsync("tab.snapshot", clipStr(label, 64), func(ctx context.Context) (any, error) {
			snap, err := b.SnapshotWithOptions(ctx, tabID, opts)
			return toPlain(snap), err
		})
	})
	_ = obj.Set("screenshot", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.screenshot({ full_page?, clip_x?, clip_y?, clip_width?, clip_height? }?)`
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		opts := browser.ScreenshotOptions{TabID: tabID}
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
				return v.rejected(err)
			}
			opts.TabID = tabID
		}
		return v.spawnAsync("tab.screenshot", clipStr(label, 64), func(ctx context.Context) (any, error) {
			data, info, err := b.Screenshot(ctx, opts)
			if err != nil {
				return nil, err
			}
			m := toPlainMap(info)
			m["__image_bytes"] = data
			m["bytes"] = len(data)
			return m, nil
		})
	})
	_ = obj.Set("eval", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.eval(script)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.eval"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		script, err := requireNonEmptyStringArg(call, 0, "script", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("tab.eval", clipStr(firstCommentOrCode(script), 72), func(ctx context.Context) (any, error) {
			res, err := b.Act(ctx, browser.Action{TabID: tabID, Kind: "evaluate", Script: script})
			if err != nil {
				return nil, err
			}
			return res.Value, nil
		})
	})
	_ = obj.Set("cdp", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.cdp(method, params?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.cdp"))
		}
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		method, err := requireNonEmptyStringArg(call, 0, "method", sig)
		if err != nil {
			return v.rejected(err)
		}
		var params map[string]any
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			if err := decodeStrictObj(call.Arguments[1], &params, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("tab.cdp", method, func(ctx context.Context) (any, error) {
			return b.RawCDP(ctx, "", tabID, method, params)
		})
	})
	_ = obj.Set("action", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.action({ kind, ref?, locator?, x?, y?, to_x?, to_y?, button?, text?, key?, scroll_x?, scroll_y?, ... })`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.action"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var act browser.Action
		if len(call.Arguments) == 0 {
			return v.rejected(errors.New("tab.action requires action object; signature: " + sig))
		}
		if err := decodeStrictObj(call.Arguments[0], &act, sig); err != nil {
			return v.rejected(err)
		}
		act.TabID = tabID
		return v.spawnAsync("tab."+act.Kind, clipStr(label, 64), func(ctx context.Context) (any, error) {
			res, err := b.Act(ctx, act)
			return toPlain(res), err
		})
	})

	_ = obj.Set("locator", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.locator(css | { role?, name?, exact?, text?, label?, placeholder?, css?, ref?, nth?, timeout_ms? })`
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		if len(call.Arguments) == 0 || goja.IsUndefined(call.Arguments[0]) || goja.IsNull(call.Arguments[0]) {
			panic(v.vm.NewTypeError("tab.locator requires CSS selector or locator object; signature: " + sig))
		}
		var loc browser.Locator
		if s, ok := call.Arguments[0].Export().(string); ok {
			loc.CSS = s
		} else if err := decodeStrictObj(call.Arguments[0], &loc, sig); err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		return v.makeLocatorHandle(tabID, loc)
	})
	_ = obj.Set("getByRole", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.getByRole(role, name? | { name?, exact?, nth?, timeout_ms? }?)`
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		role, err := requireNonEmptyStringArg(call, 0, "role", sig)
		if err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		loc := browser.Locator{Role: role}
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			if s, ok := call.Arguments[1].Export().(string); ok {
				loc.Name = s
			} else {
				if err := decodeStrictObj(call.Arguments[1], &loc, sig); err != nil {
					panic(v.vm.NewTypeError(err.Error()))
				}
				loc.Role = role
			}
		}
		return v.makeLocatorHandle(tabID, loc)
	})
	_ = obj.Set("getByText", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.getByText(text, { exact?, nth?, timeout_ms? }?)`
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		txt, err := requireNonEmptyStringArg(call, 0, "text", sig)
		if err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		loc := browser.Locator{Text: txt}
		if len(call.Arguments) > 1 {
			if err := decodeStrictObj(call.Arguments[1], &loc, sig); err != nil {
				panic(v.vm.NewTypeError(err.Error()))
			}
			loc.Text = txt
		}
		return v.makeLocatorHandle(tabID, loc)
	})
	_ = obj.Set("getByLabel", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.getByLabel(label, { exact?, nth?, timeout_ms? }?)`
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		lbl, err := requireNonEmptyStringArg(call, 0, "label", sig)
		if err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		loc := browser.Locator{Label: lbl}
		if len(call.Arguments) > 1 {
			if err := decodeStrictObj(call.Arguments[1], &loc, sig); err != nil {
				panic(v.vm.NewTypeError(err.Error()))
			}
			loc.Label = lbl
		}
		return v.makeLocatorHandle(tabID, loc)
	})
	_ = obj.Set("getByPlaceholder", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.getByPlaceholder(placeholder, { exact?, nth?, timeout_ms? }?)`
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		ph, err := requireNonEmptyStringArg(call, 0, "placeholder", sig)
		if err != nil {
			panic(v.vm.NewTypeError(err.Error()))
		}
		loc := browser.Locator{Placeholder: ph}
		if len(call.Arguments) > 1 {
			if err := decodeStrictObj(call.Arguments[1], &loc, sig); err != nil {
				panic(v.vm.NewTypeError(err.Error()))
			}
			loc.Placeholder = ph
		}
		return v.makeLocatorHandle(tabID, loc)
	})

	_ = obj.Set("click", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.click(ref | { ref?, locator?, x?, y?, button? })`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.click"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		act := browser.Action{TabID: tabID, Kind: "click"}
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			if s, ok := call.Arguments[0].Export().(string); ok {
				act.Ref = s
			} else {
				if err := decodeStrictObj(call.Arguments[0], &act, sig); err != nil {
					return v.rejected(err)
				}
				act.TabID = tabID
				act.Kind = "click"
			}
		}
		return v.spawnAsync("tab.click", firstNonEmpty(act.Ref, act.Locator.CSS, act.Locator.Name), func(ctx context.Context) (any, error) {
			res, err := b.Act(ctx, act)
			return toPlain(res), err
		})
	})
	_ = obj.Set("fill", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.fill(ref, text)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.fill"))
		}
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		ref, err := requireNonEmptyStringArg(call, 0, "ref", sig)
		if err != nil {
			return v.rejected(err)
		}
		txt, err := requireStringArg(call, 1, "text", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("tab.fill", ref, func(ctx context.Context) (any, error) {
			res, err := b.Act(ctx, browser.Action{TabID: tabID, Kind: "fill", Ref: ref, Text: txt})
			return toPlain(res), err
		})
	})
	_ = obj.Set("press", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.press(key, ref?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.press"))
		}
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		key, err := requireNonEmptyStringArg(call, 0, "key", sig)
		if err != nil {
			return v.rejected(err)
		}
		ref, err := optionalStringArg(call, 1, "ref", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("tab.press", key, func(ctx context.Context) (any, error) {
			res, err := b.Act(ctx, browser.Action{TabID: tabID, Kind: "press_key", Key: key, Ref: ref})
			return toPlain(res), err
		})
	})
	_ = obj.Set("scroll", func(call goja.FunctionCall) goja.Value {
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.scroll"))
		}
		dx, dy := 0, 0
		if len(call.Arguments) > 0 {
			dx = int(call.Arguments[0].ToInteger())
		}
		if len(call.Arguments) > 1 {
			dy = int(call.Arguments[1].ToInteger())
		}
		return v.spawnAsync("tab.scroll", fmt.Sprintf("(%d,%d)", dx, dy), func(ctx context.Context) (any, error) {
			res, err := b.Act(ctx, browser.Action{TabID: tabID, Kind: "scroll", ScrollX: dx, ScrollY: dy})
			return toPlain(res), err
		})
	})
	_ = obj.Set("handleDialog", func(call goja.FunctionCall) goja.Value {
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.handleDialog"))
		}
		accept := true
		if len(call.Arguments) > 0 {
			accept = call.Arguments[0].ToBoolean()
		}
		prompt := ""
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			prompt = call.Arguments[1].String()
		}
		return v.spawnAsync("tab.handleDialog", fmt.Sprintf("accept=%t", accept), func(ctx context.Context) (any, error) {
			res, err := b.Act(ctx, browser.Action{TabID: tabID, Kind: "handle_dialog", Accept: &accept, PromptText: prompt})
			return toPlain(res), err
		})
	})
	_ = obj.Set("network", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.network({ resource_types?, url_contains?, include_preserved?, failed_only?, limit? }?)`
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var filter browser.NetworkFilter
		if len(call.Arguments) > 0 {
			if err := decodeStrictObj(call.Arguments[0], &filter, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("tab.network", clipStr(label, 64), func(ctx context.Context) (any, error) {
			entries, err := b.Network(ctx, tabID, filter)
			return toPlain(entries), err
		})
	})
	_ = obj.Set("networkBody", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.networkBody(request_id)`
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		reqID, err := requireNonEmptyStringArg(call, 0, "request_id", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("tab.networkBody", reqID, func(ctx context.Context) (any, error) {
			res, err := b.NetworkBody(ctx, tabID, reqID)
			return toPlain(res), err
		})
	})
	_ = obj.Set("console", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.console({ levels?, limit? }?)`
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var filter browser.ConsoleFilter
		if len(call.Arguments) > 0 {
			if err := decodeStrictObj(call.Arguments[0], &filter, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("tab.console", clipStr(label, 64), func(ctx context.Context) (any, error) {
			entries, err := b.Console(ctx, tabID, filter)
			return toPlain(entries), err
		})
	})
	_ = obj.Set("emulate", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.emulate({ width?, height?, device_scale_factor?, mobile?, touch?, landscape?, clear_viewport?, cpu_throttling_rate?, network_preset?, color_scheme?, reduced_motion?, forced_colors?, user_agent?, geolocation?, extra_headers? })`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.emulate"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var opts browser.EmulateOptions
		if len(call.Arguments) > 0 {
			if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("tab.emulate", clipStr(label, 64), func(ctx context.Context) (any, error) {
			return map[string]any{"ok": true}, b.Emulate(ctx, tabID, opts)
		})
	})
	_ = obj.Set("css", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.css(cssSelector | { role?, name?, text?, css?, ref?, nth? })`
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		if len(call.Arguments) == 0 || goja.IsUndefined(call.Arguments[0]) || goja.IsNull(call.Arguments[0]) {
			return v.rejected(errors.New("tab.css requires CSS selector or locator; signature: " + sig))
		}
		var loc browser.Locator
		if s, ok := call.Arguments[0].Export().(string); ok {
			loc.CSS = s
		} else if err := decodeStrictObj(call.Arguments[0], &loc, sig); err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("tab.css", firstNonEmpty(loc.CSS, loc.Ref, loc.Name), func(ctx context.Context) (any, error) {
			res, err := b.CSS(ctx, tabID, loc)
			return toPlain(res), err
		})
	})
	_ = obj.Set("heapSnapshot", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.heapSnapshot(file_path?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.heapSnapshot"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		filePath, err := optionalStringArg(call, 0, "file_path", sig)
		if err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("tab.heapSnapshot", filePath, func(ctx context.Context) (any, error) {
			res, err := b.HeapSnapshot(ctx, tabID, filePath)
			return toPlain(res), err
		})
	})
	_ = obj.Set("logpoint", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.logpoint(url, line, expression, col?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.logpoint"))
		}
		if err := rejectExtraArgs(call, 4, sig); err != nil {
			return v.rejected(err)
		}
		if len(call.Arguments) < 3 {
			return v.rejected(errors.New("tab.logpoint requires url, line, expression; signature: " + sig))
		}
		u, err := requireNonEmptyStringArg(call, 0, "url", sig)
		if err != nil {
			return v.rejected(err)
		}
		line := int(call.Arguments[1].ToInteger())
		expr, err := requireNonEmptyStringArg(call, 2, "expression", sig)
		if err != nil {
			return v.rejected(err)
		}
		col := 0
		if len(call.Arguments) > 3 {
			col = int(call.Arguments[3].ToInteger())
		}
		return v.spawnAsync("tab.logpoint", fmt.Sprintf("%s:%d", clipStr(u, 48), line), func(ctx context.Context) (any, error) {
			id, err := b.Logpoint(ctx, tabID, u, line, col, expr)
			return map[string]any{"breakpoint_id": id}, err
		})
	})
	_ = obj.Set("screencastStart", func(call goja.FunctionCall) goja.Value {
		const sig = `tab.screencastStart({ max_width?, max_height?, quality? }?)`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.screencastStart"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var opts struct {
			MaxWidth  int `json:"max_width"`
			MaxHeight int `json:"max_height"`
			Quality   int `json:"quality"`
		}
		if len(call.Arguments) > 0 {
			if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("tab.screencastStart", label, func(ctx context.Context) (any, error) {
			return map[string]any{"recording": true}, b.ScreencastStart(ctx, tabID, opts.MaxWidth, opts.MaxHeight, opts.Quality)
		})
	})
	_ = obj.Set("screencastStop", func(call goja.FunctionCall) goja.Value {
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids tab.screencastStop"))
		}
		return v.spawnAsync("tab.screencastStop", label, func(ctx context.Context) (any, error) {
			frames, err := b.ScreencastStop(ctx, tabID)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, len(frames))
			for i, f := range frames {
				out[i] = map[string]any{
					"seq":           f.Seq,
					"timestamp":     f.Timestamp,
					"width":         f.Width,
					"height":        f.Height,
					"mime_type":     f.MIMEType,
					"bytes":         len(f.Data),
					"__image_bytes": f.Data,
				}
			}
			return out, nil
		})
	})
	return obj
}

func (v *sessionVM) makeLocatorHandle(tabID string, loc browser.Locator) goja.Value {
	b := v.deps.Browser
	obj := v.vm.NewObject()
	desc := firstNonEmpty(loc.CSS, loc.Role+":"+loc.Name, loc.Text, loc.Label, loc.Placeholder, loc.Ref)

	act := func(kind string, mut func(*browser.Action)) goja.Value {
		if v.isReadOnly() && kind != "wait_for" {
			return v.rejected(errors.New("read-only grant forbids locator." + kind))
		}
		a := browser.Action{TabID: tabID, Kind: kind, Locator: loc}
		if mut != nil {
			mut(&a)
		}
		return v.spawnAsync("locator."+kind, clipStr(desc, 64), func(ctx context.Context) (any, error) {
			res, err := b.Act(ctx, a)
			return toPlain(res), err
		})
	}

	_ = obj.Set("click", func(call goja.FunctionCall) goja.Value { return act("click", nil) })
	_ = obj.Set("doubleClick", func(call goja.FunctionCall) goja.Value { return act("double_click", nil) })
	_ = obj.Set("hover", func(call goja.FunctionCall) goja.Value { return act("hover", nil) })
	_ = obj.Set("fill", func(call goja.FunctionCall) goja.Value {
		txt := ""
		if len(call.Arguments) > 0 {
			txt = call.Arguments[0].String()
		}
		return act("fill", func(a *browser.Action) { a.Text = txt })
	})
	_ = obj.Set("type", func(call goja.FunctionCall) goja.Value {
		txt := ""
		if len(call.Arguments) > 0 {
			txt = call.Arguments[0].String()
		}
		return act("type_text", func(a *browser.Action) { a.Text = txt })
	})
	_ = obj.Set("press", func(call goja.FunctionCall) goja.Value {
		key := ""
		if len(call.Arguments) > 0 {
			key = call.Arguments[0].String()
		}
		return act("press_key", func(a *browser.Action) { a.Key = key })
	})
	_ = obj.Set("select", func(call goja.FunctionCall) goja.Value {
		opt := ""
		if len(call.Arguments) > 0 {
			opt = call.Arguments[0].String()
		}
		return act("select", func(a *browser.Action) { a.Option = opt })
	})
	_ = obj.Set("check", func(call goja.FunctionCall) goja.Value {
		checked := true
		if len(call.Arguments) > 0 {
			checked = call.Arguments[0].ToBoolean()
		}
		return act("check", func(a *browser.Action) { a.Checked = &checked })
	})
	_ = obj.Set("uploadFiles", func(call goja.FunctionCall) goja.Value {
		const sig = `locator.uploadFiles(pathsArray)`
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var files []string
		if len(call.Arguments) == 0 {
			return v.rejected(errors.New("locator.uploadFiles requires paths array; signature: " + sig))
		}
		if err := decodeStrictSlice(call.Arguments[0], &files, "files", sig); err != nil {
			return v.rejected(err)
		}
		return act("upload_files", func(a *browser.Action) { a.Files = files })
	})
	_ = obj.Set("waitFor", func(call goja.FunctionCall) goja.Value {
		const sig = `locator.waitFor({ timeout_ms? }?)`
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		timeoutMS := loc.TimeoutMS
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			var opts struct {
				TimeoutMS int `json:"timeout_ms"`
			}
			if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
				return v.rejected(err)
			}
			if opts.TimeoutMS > 0 {
				timeoutMS = opts.TimeoutMS
			}
		}
		return act("wait_for", func(a *browser.Action) { a.Locator.TimeoutMS = timeoutMS })
	})
	_ = obj.Set("nth", func(call goja.FunctionCall) goja.Value {
		next := loc
		if len(call.Arguments) > 0 {
			next.Nth = int(call.Arguments[0].ToInteger())
		}
		return v.makeLocatorHandle(tabID, next)
	})
	return obj
}

func (v *sessionVM) buildDesktopSDK() goja.Value {
	obj := v.vm.NewObject()
	ctrl := v.deps.Computer

	_ = obj.Set("targets", func(call goja.FunctionCall) goja.Value {
		const sig = `desktop.targets({ pid?, title?, process? }?)`
		if ctrl == nil {
			return v.rejected(errors.New("desktop controller unavailable"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var opts computer.TargetsOptions
		if len(call.Arguments) > 0 {
			if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("desktop.targets", firstNonEmpty(opts.Process, opts.Title, "all"), func(ctx context.Context) (any, error) {
			res, err := ctrl.Targets(ctx, opts)
			return toPlain(res), err
		})
	})
	_ = obj.Set("state", func(call goja.FunctionCall) goja.Value {
		const sig = `desktop.state({ control_id?, target_id?, display_id?, region?, image?, cursor?, elements?, name?, role?, automation_id?, max_elements?, max_width?, max_height? }?)`
		if ctrl == nil {
			return v.rejected(errors.New("desktop controller unavailable"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var opts computer.StateOptions
		if len(call.Arguments) > 0 {
			if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("desktop.state", firstNonEmpty(opts.TargetID, opts.DisplayID, "desktop"), func(ctx context.Context) (any, error) {
			data, st, err := ctrl.State(ctx, opts)
			if err != nil {
				return nil, err
			}
			m := toPlainMap(st)
			if len(data) > 0 {
				m["__image_bytes"] = data
				m["bytes"] = len(data)
			}
			return m, nil
		})
	})
	_ = obj.Set("wait", func(call goja.FunctionCall) goja.Value {
		const sig = `desktop.wait({ target_id?, display_id?, name?, role?, automation_id?, timeout_seconds?, image? })`
		if ctrl == nil {
			return v.rejected(errors.New("desktop controller unavailable"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var opts computer.WaitOptions
		if len(call.Arguments) > 0 {
			if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("desktop.wait", firstNonEmpty(opts.Name, opts.AutomationID, opts.Role), func(ctx context.Context) (any, error) {
			data, st, err := ctrl.Wait(ctx, opts)
			if err != nil {
				return nil, err
			}
			m := toPlainMap(st)
			if len(data) > 0 {
				m["__image_bytes"] = data
			}
			return m, nil
		})
	})
	_ = obj.Set("action", func(call goja.FunctionCall) goja.Value {
		const sig = `desktop.action({ kind, control_id?, target_id?, element_ref?, x?, y?, to_x?, to_y?, button?, text?, key?, keys?, scroll_x?, scroll_y? })`
		if v.isReadOnly() {
			return v.rejected(errors.New("read-only grant forbids desktop.action"))
		}
		if ctrl == nil {
			return v.rejected(errors.New("desktop controller unavailable"))
		}
		if err := rejectExtraArgs(call, 1, sig); err != nil {
			return v.rejected(err)
		}
		var act computer.Action
		if len(call.Arguments) > 0 {
			if err := decodeStrictObj(call.Arguments[0], &act, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("desktop."+act.Kind, firstNonEmpty(act.ElementRef, act.TargetID), func(ctx context.Context) (any, error) {
			res, err := ctrl.Act(ctx, act)
			return toPlain(res), err
		})
	})
	_ = obj.Set("control", func(call goja.FunctionCall) goja.Value {
		const sig = `desktop.control("status"|"acquire"|"release", { control_id?, label? }?)`
		if ctrl == nil {
			return v.rejected(errors.New("desktop controller unavailable"))
		}
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		var opts computer.ControlOptions
		if len(call.Arguments) > 0 {
			if s, ok := call.Arguments[0].Export().(string); ok {
				opts.Kind = s
				if len(call.Arguments) > 1 {
					if err := decodeStrictObj(call.Arguments[1], &opts, sig); err != nil {
						return v.rejected(err)
					}
					opts.Kind = s
				}
			} else {
				if err := rejectExtraArgs(call, 1, sig); err != nil {
					return v.rejected(err)
				}
				if err := decodeStrictObj(call.Arguments[0], &opts, sig); err != nil {
					return v.rejected(err)
				}
			}
		}
		if v.isReadOnly() && opts.Kind != "status" {
			return v.rejected(errors.New("read-only grant forbids desktop.control " + opts.Kind))
		}
		return v.spawnAsync("desktop.control", opts.Kind, func(ctx context.Context) (any, error) {
			res, err := ctrl.Control(ctx, opts)
			return toPlain(res), err
		})
	})
	return obj
}

func (v *sessionVM) buildImageSDK() goja.Value {
	obj := v.vm.NewObject()
	_ = obj.Set("read", func(call goja.FunctionCall) goja.Value {
		const sig = `img.read(path, { max_bytes?, max_pixels?, crop_x?, crop_y?, crop_width?, crop_height?, max_width?, max_height? }?)`
		if err := rejectExtraArgs(call, 2, sig); err != nil {
			return v.rejected(err)
		}
		path, err := requireNonEmptyStringArg(call, 0, "path", sig)
		if err != nil {
			return v.rejected(err)
		}
		var opts runtimeimage.ReadOptions
		if len(call.Arguments) > 1 {
			if err := decodeStrictObj(call.Arguments[1], &opts, sig); err != nil {
				return v.rejected(err)
			}
		}
		return v.spawnAsync("img.read", path, func(ctx context.Context) (any, error) {
			data, meta, err := runtimeimage.Read(ctx, path, opts)
			if err != nil {
				return nil, err
			}
			m := toPlainMap(meta)
			m["__image_bytes"] = data
			m["bytes"] = len(data)
			return m, nil
		})
	})
	return obj
}

func (v *sessionVM) buildCapsSDK() goja.Value {
	obj := v.vm.NewObject()
	reg := v.deps.Capabilities
	_ = obj.Set("list", func(call goja.FunctionCall) goja.Value {
		const sig = `caps.list()`
		if err := rejectExtraArgs(call, 0, sig); err != nil {
			return v.rejected(err)
		}
		return v.spawnAsync("caps.list", "", func(_ context.Context) (any, error) {
			if reg == nil {
				return []any{}, nil
			}
			defs := reg.List()
			if v.deps.LocalApps {
				defs = append(defs, reg.AppDefinitions()...)
			}
			return toPlain(defs), nil
		})
	})
	_ = obj.Set("call", func(call goja.FunctionCall) goja.Value {
		const sig = `caps.call(name, args?, browser_id?, tab_id?)`
		if reg == nil {
			return v.rejected(errors.New("capabilities registry unavailable"))
		}
		if err := rejectExtraArgs(call, 4, sig); err != nil {
			return v.rejected(err)
		}
		name, err := requireNonEmptyStringArg(call, 0, "name", sig)
		if err != nil {
			return v.rejected(err)
		}
		args := map[string]any{}
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			if err := decodeStrictObj(call.Arguments[1], &args, sig); err != nil {
				return v.rejected(err)
			}
		}
		browserID, err := optionalStringArg(call, 2, "browser_id", sig)
		if err != nil {
			return v.rejected(err)
		}
		tabID, err := optionalStringArg(call, 3, "tab_id", sig)
		if err != nil {
			return v.rejected(err)
		}
		if v.isReadOnly() {
			allowed := false
			for _, d := range reg.List() {
				if d.Name == name && d.Annotations.ReadOnlyHint && !d.Annotations.DestructiveHint {
					allowed = true
				}
			}
			if !allowed {
				return v.rejected(fmt.Errorf("read-only grant forbids capability %q", name))
			}
		}
		return v.spawnAsync("caps.call", name, func(ctx context.Context) (any, error) {
			if v.deps.LocalApps {
				for _, d := range reg.AppDefinitions() {
					if d.Name == name {
						return reg.CallApp(ctx, name, args)
					}
				}
			}
			return reg.Call(ctx, name, browserID, tabID, args)
		})
	})
	return obj
}

func requireStringArg(call goja.FunctionCall, idx int, param, sig string) (string, error) {
	if idx >= len(call.Arguments) || goja.IsUndefined(call.Arguments[idx]) || goja.IsNull(call.Arguments[idx]) {
		return "", fmt.Errorf("missing required string argument %q; signature: %s", param, sig)
	}
	s, ok := call.Arguments[idx].Export().(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string, got %T; signature: %s", param, call.Arguments[idx].Export(), sig)
	}
	return s, nil
}

func requireNonEmptyStringArg(call goja.FunctionCall, idx int, param, sig string) (string, error) {
	s, err := requireStringArg(call, idx, param, sig)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("argument %q cannot be empty; signature: %s", param, sig)
	}
	return s, nil
}

func optionalStringArg(call goja.FunctionCall, idx int, param, sig string) (string, error) {
	if idx >= len(call.Arguments) || goja.IsUndefined(call.Arguments[idx]) || goja.IsNull(call.Arguments[idx]) {
		return "", nil
	}
	s, ok := call.Arguments[idx].Export().(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string, got %T; signature: %s", param, call.Arguments[idx].Export(), sig)
	}
	return s, nil
}

func rejectExtraArgs(call goja.FunctionCall, maxArgs int, sig string) error {
	actual := len(call.Arguments)
	for actual > maxArgs && goja.IsUndefined(call.Arguments[actual-1]) {
		actual--
	}
	if actual > maxArgs {
		return fmt.Errorf("expected at most %d argument(s), got %d; signature: %s", maxArgs, actual, sig)
	}
	return nil
}

func decodeStrictObj(val goja.Value, target any, sig string) error {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil
	}
	exp := val.Export()
	if _, ok := exp.(map[string]any); !ok {
		return fmt.Errorf("expected options object, got %T; signature: %s", exp, sig)
	}
	b, err := json.Marshal(exp)
	if err != nil {
		return fmt.Errorf("invalid options (%v); signature: %s", err, sig)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("invalid options (%v); signature: %s", err, sig)
	}
	return nil
}

func decodeStrictSlice(val goja.Value, target any, param, sig string) error {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil
	}
	exp := val.Export()
	if _, ok := exp.([]any); !ok {
		return fmt.Errorf("argument %q must be an array, got %T; signature: %s", param, exp, sig)
	}
	b, err := json.Marshal(exp)
	if err != nil {
		return fmt.Errorf("invalid %s (%v); signature: %s", param, err, sig)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("invalid %s (%v); signature: %s", param, err, sig)
	}
	return nil
}

func toPlain(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

func toPlainMap(v any) map[string]any {
	if m, ok := toPlain(v).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func formatJSArg(val goja.Value) string {
	if val == nil || goja.IsUndefined(val) {
		return "undefined"
	}
	if goja.IsNull(val) {
		return "null"
	}
	exp := val.Export()
	switch x := exp.(type) {
	case string:
		return x
	case bool, int, int64, float64:
		return fmt.Sprint(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return val.String()
		}
		return string(b)
	}
}

func firstNonEmpty(values ...string) string {
	for _, s := range values {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func clipStr(s string, maxLen int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " ")
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxLen {
		return string(r)
	}
	return string(r[:maxLen]) + "…"
}

func firstCommentOrCode(script string) string {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "//")
		return strings.TrimSpace(line)
	}
	return script
}
