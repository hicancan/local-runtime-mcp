package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestBundledWorkerHandshakeMatchesHostRelease(t *testing.T) {
	manifestBytes, err := extensionFiles.ReadFile("extension/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	versionBytes, err := extensionFiles.ReadFile("extension/version.js")
	if err != nil {
		t.Fatal("the final bundled worker must include its generated version module:", err)
	}
	match := regexp.MustCompile(`export const VERSION\s*=\s*["']([^"']+)["']`).FindSubmatch(versionBytes)
	if len(match) != 2 || string(match[1]) != ExtensionVersion || manifest.Version != ExtensionVersion {
		t.Fatalf("host, manifest and bundled handshake version disagree: host=%s manifest=%s", ExtensionVersion, manifest.Version)
	}
	worker, err := extensionFiles.ReadFile("extension/service_worker.js")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`import\s*\{\s*VERSION\s*\}\s*from\s*["']\./version\.js["']`).Match(worker) ||
		!regexp.MustCompile(`extension_version:\s*VERSION\b`).Match(worker) ||
		regexp.MustCompile(`(?:const|let|var)\s+VERSION\s*=`).Match(worker) {
		t.Fatal("the embedded service worker must send the imported version, without a second handshake constant")
	}
	// This checks the source of the generated assets too, catching a version
	// bump followed by a Go build that forgot the extension generation step.
	pkg, err := os.ReadFile(filepath.Join("..", "..", "browser-extension", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(pkg, &source); err != nil || source.Version != ExtensionVersion {
		t.Fatal("generate the host and extension from the package release version before building")
	}
}
