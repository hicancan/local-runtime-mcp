package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledExtensionCarriesItsPanelIconsAndLanguages(t *testing.T) {
	dir, err := InstallExtension(filepath.Join(t.TempDir(), "extension"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "popup.html", "popup.js", "icons/icon-128.png", "_locales/en/messages.json", "_locales/zh_CN/messages.json"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	var en, zh map[string]any
	for path, into := range map[string]*map[string]any{"_locales/en/messages.json": &en, "_locales/zh_CN/messages.json": &zh} {
		data, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err := json.Unmarshal(data, into); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	for key := range en {
		if _, ok := zh[key]; !ok {
			t.Fatalf("Chinese lacks %q", key)
		}
	}
}
