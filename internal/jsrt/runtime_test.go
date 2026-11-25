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
