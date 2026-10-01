//go:build windows && amd64

package computer

import (
	"bytes"
	"debug/pe"
	"strings"
	"testing"
)

func TestEmbeddedWorkerPortableRuntime(t *testing.T) {
	data, err := workerExecutable.ReadFile("worker_windows_amd64.exe")
	if err != nil {
		t.Fatal(err)
	}
	image, err := pe.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	dependencies, err := image.ImportedLibraries()
	if err != nil {
		t.Fatal(err)
	}
	for _, dependency := range dependencies {
		name := strings.ToLower(dependency)
		for _, prefix := range []string{"vcruntime", "msvcp", "msvcr", "concrt"} {
			if strings.HasPrefix(name, prefix) {
				t.Fatalf("embedded worker requires compiler redistributable %q; rebuild with scripts/build-native.ps1", dependency)
			}
		}
	}
}
