package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestConcurrentSameVersionOnlyOneCommit(t *testing.T) {
	service := New()
	path := filepath.Join(t.TempDir(), "shared.txt")
	created, err := service.WriteText(context.Background(), path, "base", WriteTextOptions{Mode: "create"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, content := range []string{"first", "second"} {
		go func() {
			<-start
			_, err := service.WriteText(context.Background(), path, content, WriteTextOptions{Mode: "replace", ExpectedSHA256: created.SHA256})
			results <- err
		}()
	}
	close(start)
	success := 0
	for range 2 {
		if <-results == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("same-version commits succeeded %d times", success)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.paths) != 0 {
		t.Fatal("commit locks were retained")
	}
}

func TestCreateOnlyCommitNeverOverwritesExternalRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.txt")
	if err := os.WriteFile(path, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(context.Background(), path, []byte("runtime"), 0o644, true); err == nil {
		t.Fatal("create-only OS commit overwrote existing file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "external" {
		t.Fatalf("destination = %q, %v", data, err)
	}
}

func TestDifferentPathsDoNotWaitForBusyPath(t *testing.T) {
	service := New()
	root := t.TempDir()
	busy := filepath.Join(root, "busy.txt")
	unlock, err := service.acquire(context.Background(), busy)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := service.WriteText(context.Background(), filepath.Join(root, "other.txt"), "other", WriteTextOptions{Mode: "create"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := service.WriteText(ctx, busy, "canceled", WriteTextOptions{Mode: "create"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancel = %v", err)
	}
	if _, err := os.Stat(busy); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled commit changed a file")
	}
}

func TestIndependentServicesStillHaveNoOverwriteCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "create.txt")
	results := make(chan error, 16)
	var calls sync.WaitGroup
	for range 16 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			_, err := New().WriteText(context.Background(), path, "complete", WriteTextOptions{Mode: "create"})
			results <- err
		}()
	}
	calls.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("create published %d files", succeeded)
	}
}

func TestWindowsCaseAliasesShareCommitIdentity(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path case")
	}
	path := filepath.Join(t.TempDir(), "CaseFile.txt")
	first, err := commitIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := commitIdentity(filepath.Join(filepath.Dir(path), "casefile.TXT"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("case aliases differed: %q != %q", first, second)
	}
}

func TestReplaceRequiresExplicitVersion(t *testing.T) {
	service := New()
	path := filepath.Join(t.TempDir(), "replace.txt")
	if _, err := service.WriteText(context.Background(), path, "data", WriteTextOptions{}); err == nil {
		t.Fatal("implicit write mode accepted")
	}
	if _, err := service.WriteText(context.Background(), path, "data", WriteTextOptions{Mode: "replace"}); err == nil {
		t.Fatal("replace without version accepted")
	}
}
