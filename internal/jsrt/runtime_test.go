package jsrt

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
)

func TestStatefulREPLPersistenceAndParallelPromises(t *testing.T) {
	ctx := context.Background()
	procMgr := runtimeprocess.NewManager(ctx)
	defer procMgr.Close()
	eng := NewEngine(ctx, Dependencies{
		Process:    procMgr,
		Filesystem: filesystem.New(),
	})
	defer eng.Close()

	// Turn 1: declare top-level variables & helper function, run 3 sleeps in parallel.
	start := time.Now()
	r1, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-1",
		Title:     "并发测试三个异步任务",
		DoneTitle: "已完成并发测试",
		Code: `
			const items = ['alpha', 'beta', 'gamma'];
			async function delayedTag(name) {
				await sleep(120);
				return name.toUpperCase();
			}
			const results = await Promise.allSettled(items.map(delayedTag));
			let summary = results.map(x => x.value).join(',');
			print('parallel:', summary);
			return summary;
		`,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run 1 failed: %v", err)
	}
	if r1.Status != "done" || r1.Error != "" {
		t.Fatalf("unexpected r1 status=%s err=%s", r1.Status, r1.Error)
	}
	if r1.Value != "ALPHA,BETA,GAMMA" {
		t.Fatalf("unexpected r1 value: %#v", r1.Value)
	}
	if !strings.Contains(r1.Output, "parallel: ALPHA,BETA,GAMMA") {
		t.Fatalf("unexpected r1 output: %q", r1.Output)
	}
	// 3 x 120ms in parallel must finish well under 300ms (proves real concurrency, not 360ms serial).
	if elapsed > 300*time.Millisecond {
		t.Fatalf("expected parallel execution (<300ms), took %v", elapsed)
	}

	// Turn 2: reference top-level `items`, `summary`, and `delayedTag` from Turn 1!
	r2, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-1",
		Title:     "验证跨轮变量持久性",
		Code: `
			const tagged = await delayedTag('delta');
			return { count: items.length, prev: summary, next: tagged };
		`,
	})
	if err != nil {
		t.Fatalf("Run 2 failed: %v", err)
	}
	if r2.Status != "done" {
		t.Fatalf("unexpected r2 status=%s err=%s", r2.Status, r2.Error)
	}
	m, ok := r2.Value.(map[string]any)
	if !ok || m["prev"] != "ALPHA,BETA,GAMMA" || m["next"] != "DELTA" {
		t.Fatalf("cross-turn state lost: %#v", r2.Value)
	}
}

func TestAsyncCellYieldWaitAndTerminate(t *testing.T) {
	ctx := context.Background()
	eng := NewEngine(ctx, Dependencies{Filesystem: filesystem.New()})
	defer eng.Close()

	// Yield early while still running
	r1, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-wait",
		Title:     "后台长任务",
		YieldMS:   60,
		Code: `
			print('step1');
			await sleep(200);
			print('step2');
			return 'finished-ok';
		`,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if r1.Status != "running" || r1.CellID == "" {
		t.Fatalf("expected running cell, got %#v", r1)
	}

	// Wait until done
	r2, err := eng.Wait(ctx, WaitOptions{CellID: r1.CellID, YieldMS: 2000})
	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}
	if r2.Status != "done" || r2.Value != "finished-ok" {
		t.Fatalf("expected done result, got %#v", r2)
	}
	if !strings.Contains(r2.Output, "step2") {
		t.Fatalf("expected incremental output step2, got %q", r2.Output)
	}

	// Test cancellation via wait(terminate=true)
	r3, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-wait",
		Title:     "可取消任务",
		YieldMS:   50,
		Code:      `await sleep(10000); return 'never';`,
	})
	if err != nil || r3.Status != "running" {
		t.Fatalf("expected running cell, got %#v err=%v", r3, err)
	}
	r4, err := eng.Wait(ctx, WaitOptions{CellID: r3.CellID, Terminate: true})
	if err != nil || r4.Status != "canceled" {
		t.Fatalf("expected canceled cell, got %#v err=%v", r4, err)
	}
}

func TestFSProcImageAndReadOnlyEnforcement(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	procMgr := runtimeprocess.NewManager(ctx)
	defer procMgr.Close()
	eng := NewEngine(ctx, Dependencies{
		Process:    procMgr,
		Filesystem: filesystem.New(),
	})
	defer eng.Close()

	// Create a tiny valid PNG to verify img.read + emitImage
	imgPath := filepath.Join(dir, "sample.png")
	im := image.NewRGBA(image.Rect(0, 0, 8, 8))
	im.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imgPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	targetFile := filepath.Join(dir, "hello.txt")
	r, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-io",
		Title:     "测试文件读写与图片透传",
		Code: `
			const file = ` + "`" + filepath.ToSlash(targetFile) + "`" + `;
			const imgFile = ` + "`" + filepath.ToSlash(imgPath) + "`" + `;
			const w = await fs.write(file, "line1\nline2\n", { mode: "create" });
			const rd = await fs.read(file, { with_sha256: true });
			const shot = await img.read(imgFile);
			emitImage(shot, "sample");
			return { shaMatch: w.sha256 === rd.sha256, content: rd.content, imgW: shot.width };
		`,
	})
	if err != nil || r.Status != "done" {
		t.Fatalf("Run failed: status=%s err=%s goErr=%v", r.Status, r.Error, err)
	}
	if len(r.Images) != 1 || r.Images[0].MIMEType != "image/png" || r.Images[0].Width != 8 {
		t.Fatalf("expected 1 emitted PNG image, got %#v", r.Images)
	}
	if len(r.SubOps) < 3 {
		t.Fatalf("expected sub_ops telemetry recorded, got %#v", r.SubOps)
	}

	// Read-only enforcement: fs.read allowed, fs.write and proc.run rejected.
	roRead, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-ro",
		Title:     "只读读文件",
		ReadOnly:  true,
		Code:      `const rd = await fs.read(` + "`" + filepath.ToSlash(targetFile) + "`" + `); return rd.content;`,
	})
	if err != nil || roRead.Status != "done" {
		t.Fatalf("read-only fs.read should succeed, got %#v err=%v", roRead, err)
	}
	roWrite, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-ro",
		Title:     "只读写文件应拦截",
		ReadOnly:  true,
		Code:      `await fs.write(` + "`" + filepath.ToSlash(targetFile) + "`" + `, "bad", { mode: "create" });`,
	})
	if err != nil || roWrite.Status != "error" || !strings.Contains(roWrite.Error, "read-only") {
		t.Fatalf("read-only fs.write should fail, got %#v err=%v", roWrite, err)
	}
}

func TestRewritePreservesForLoopsAndStrings(t *testing.T) {
	ctx := context.Background()
	eng := NewEngine(ctx, Dependencies{})
	defer eng.Close()

	r, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-rw",
		Title:     "测试词法提升不破坏块级作用域",
		Code: `
			const [a, b] = [10, 20];
			const { x, y: z } = { x: 3, y: 4 };
			let sum = 0;
			for (let i = 1; i <= 5; i++) {
				const sq = i * i;
				sum += sq;
			}
			const s = "const fake = 1; let nope = 2;";
			return { a, b, x, z, sum, s };
		`,
	})
	if err != nil || r.Status != "done" {
		t.Fatalf("rewrite test failed: status=%s err=%s goErr=%v", r.Status, r.Error, err)
	}
	m := r.Value.(map[string]any)
	if m["sum"] != float64(55) || m["a"] != float64(10) || m["z"] != float64(4) {
		t.Fatalf("unexpected rewrite result: %#v", m)
	}
}

func TestStrictValidationReplAutoReturnHelpAndSubOpErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sample := filepath.Join(dir, "lines.txt")
	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, "line-"+string(rune('0'+i/10))+string(rune('0'+i%10)))
	}
	if err := os.WriteFile(sample, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(ctx, Dependencies{Filesystem: filesystem.New()})
	defer eng.Close()

	// 1. fs.read with start_line + end_line AND bare expression REPL auto-return.
	r1, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-strict",
		Title:     "验证行范围与末表达式自动返回",
		Code:      `await fs.read(` + "`" + filepath.ToSlash(sample) + "`" + `, { start_line: 3, end_line: 7 })`,
	})
	if err != nil || r1.Status != "done" {
		t.Fatalf("fs.read range failed: %#v err=%v", r1, err)
	}
	m1, ok := r1.Value.(map[string]any)
	if !ok || m1["start_line"] != float64(3) || m1["end_line"] != float64(7) {
		t.Fatalf("expected lines 3..7 via REPL auto-return, got %#v", r1.Value)
	}

	// 2. Unknown field, extra positional args, and object-as-path must fail fast with signature hints.
	r2, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-strict",
		Title:     "验证非法参数立即报错",
		Code: `
			const f = ` + "`" + filepath.ToSlash(sample) + "`" + `;
			const res = await Promise.allSettled([
				fs.read(f, { startLine: 1, endLine: 5 }),
				fs.read(f, 1, 5),
				fs.read({ path: f, start_line: 1, end_line: 5 }),
				caps.list(),
			]);
			res.map(x => x.status === 'rejected' ? String(x.reason) : 'ok')
		`,
	})
	if err != nil || r2.Status != "done" {
		t.Fatalf("strict validation cell failed: %#v err=%v", r2, err)
	}
	arr, ok := r2.Value.([]any)
	if !ok || len(arr) != 4 {
		t.Fatalf("expected 4 results, got %#v", r2.Value)
	}
	for i := 0; i < 3; i++ {
		s, _ := arr[i].(string)
		if !strings.Contains(s, "signature:") {
			t.Fatalf("case %d expected signature hint in error, got %q", i, s)
		}
	}
	if arr[3] != "ok" {
		t.Fatalf("expected caps.list ok, got %#v", arr[3])
	}
	// caps.list must be recorded in sub_ops.
	foundCaps := false
	for _, op := range r2.SubOps {
		if op.Op == "caps.list" {
			foundCaps = true
		}
	}
	if !foundCaps {
		t.Fatalf("expected caps.list in sub_ops, got %#v", r2.SubOps)
	}
	// All 3 parameter validation errors must also be recorded in sub_ops and counted in sub_op_errors!
	if r2.SubOpErrors != 3 {
		t.Fatalf("expected 3 recorded validation errors in sub_op_errors, got %d (%#v)", r2.SubOpErrors, r2.SubOps)
	}

	// 3. sub_op_errors reports failed async sub-ops inside a done cell.
	r3, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-strict",
		Title:     "验证子操作失败计数",
		Code: `
			await Promise.allSettled([
				fs.read(` + "`" + filepath.ToSlash(filepath.Join(dir, "missing.txt")) + "`" + `),
				caps.list(),
			]);
			"done"
		`,
	})
	if err != nil || r3.Status != "done" || r3.SubOpErrors != 1 {
		t.Fatalf("expected status=done sub_op_errors=1, got %#v err=%v", r3, err)
	}

	// 4. fs.write expected_sha256 JSON tag actually enforces optimistic lock.
	r4, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-strict",
		Title:     "验证 expected_sha256 乐观锁",
		Code: `
			await fs.write(` + "`" + filepath.ToSlash(sample) + "`" + `, "overwrite", { mode: "replace", expected_sha256: "0000000000000000000000000000000000000000000000000000000000000000" });
		`,
	})
	if err != nil || r4.Status != "error" || !strings.Contains(strings.ToLower(r4.Error), "sha256") {
		t.Fatalf("expected sha256 mismatch rejection, got %#v err=%v", r4, err)
	}

	// 5. Built-in help() returns signature docs via REPL auto-return.
	r5, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-strict",
		Title:     "验证内置 help 文档",
		Code:      `help("fs.read")`,
	})
	if err != nil || r5.Status != "done" {
		t.Fatalf("help failed: %#v err=%v", r5, err)
	}
	doc, _ := r5.Value.(string)
	if !strings.Contains(doc, "end_line") || !strings.Contains(doc, "fs.read") {
		t.Fatalf("unexpected help doc: %q", doc)
	}
}

func TestASTPersistenceClassRedeclareAndWebGlobals(t *testing.T) {
	ctx := context.Background()
	eng := NewEngine(ctx, Dependencies{})
	defer eng.Close()

	// Turn 1: class, nested destructuring, const declaration, and Web globals.
	r1, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-ast",
		Title:     "验证 AST 持久化与标准 Web 全局对象",
		Code: `
			class Counter {
				constructor(n) { this.n = n; }
				inc(by = 1) { this.n += by; return this.n; }
			}
			const { meta: { tag }, nums: [first, ...rest] } = { meta: { tag: "v18" }, nums: [10, 20, 30] };
			const c = new Counter(first);
			const u = new URL("https://example.com/a/b?x=1&y=two#top");
			u.searchParams.set("z", "3");
			const encoded = btoa("hello-lrmcp");
			const decoded = atob(encoded);
			const timed = await new Promise((resolve) => {
				const skip = setTimeout(() => resolve("cancelled"), 5);
				clearTimeout(skip);
				setTimeout((v) => resolve(v), 15, "timer-ok");
			});
			({ tag, first, restLen: rest.length, val: c.inc(5), href: u.href, y: u.searchParams.get("y"), decoded, timed })
		`,
	})
	if err != nil || r1.Status != "done" {
		t.Fatalf("AST turn 1 failed: status=%s err=%s goErr=%v", r1.Status, r1.Error, err)
	}
	m1, ok := r1.Value.(map[string]any)
	if !ok || m1["tag"] != "v18" || m1["val"] != float64(15) || m1["decoded"] != "hello-lrmcp" || m1["timed"] != "timer-ok" || m1["y"] != "two" {
		t.Fatalf("unexpected AST turn 1 value: %#v", r1.Value)
	}
	href, _ := m1["href"].(string)
	if !strings.Contains(href, "z=3") || !strings.HasSuffix(href, "#top") {
		t.Fatalf("unexpected URL href: %q", href)
	}

	// Turn 2: reuse class Counter, nested destructured vars, AND re-declare const `tag` cleanly.
	r2, err := eng.Run(ctx, RunOptions{
		SessionID: "sess-ast",
		Title:     "验证跨轮复用类与重声明同名 const",
		Code: `
			const tag = "v18.2";
			const c2 = new Counter(100);
			({ prevCounter: c.inc(2), newCounter: c2.inc(3), restSum: rest[0] + rest[1], tag })
		`,
	})
	if err != nil || r2.Status != "done" {
		t.Fatalf("AST turn 2 failed: status=%s err=%s goErr=%v", r2.Status, r2.Error, err)
	}
	m2, ok := r2.Value.(map[string]any)
	if !ok || m2["prevCounter"] != float64(17) || m2["newCounter"] != float64(103) || m2["restSum"] != float64(50) || m2["tag"] != "v18.2" {
		t.Fatalf("unexpected AST turn 2 value: %#v", r2.Value)
	}
}
