package modelroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingAndEmptyCatalogPreserveProviderOnlyRoutes(t *testing.T) {
	t.Parallel()

	missing, err := Load(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("Load(missing) error = %v", err)
	}
	if got := missing.Resolve("glm"); got != (Route{Provider: "glm"}) {
		t.Fatalf("Resolve(missing) = %#v, want provider-only route", got)
	}

	emptyDir := t.TempDir()
	writeModels(t, emptyDir, `{}`)
	empty, err := Load(emptyDir)
	if err != nil {
		t.Fatalf("Load(empty) error = %v", err)
	}
	if got := empty.Resolve("sub2api"); got != (Route{Provider: "sub2api"}) {
		t.Fatalf("Resolve(empty) = %#v, want provider-only route", got)
	}
}

func TestLoadResolvesLiteralProviderModelRoutes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	writeModels(t, directory, `{
  "openai": "gpt-5.6-sol",
  "sub2api": "gpt-5.6-sol",
  "glm": "glm-5.2"
}`)

	catalog, err := Load(directory)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	wants := map[string]Route{
		"openai":   {Provider: "openai", Model: "gpt-5.6-sol"},
		"sub2api":  {Provider: "sub2api", Model: "gpt-5.6-sol"},
		"glm":      {Provider: "glm", Model: "glm-5.2"},
		"unmapped": {Provider: "unmapped"},
		"":         {},
	}
	for provider, want := range wants {
		if got := catalog.Resolve(provider); got != want {
			t.Errorf("Resolve(%q) = %#v, want %#v", provider, got, want)
		}
	}
}

func TestLoadRejectsUnsafeCatalogsWithoutLeakingValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
	}{
		{name: "top-level array", data: `["secret-model"]`},
		{name: "duplicate provider", data: `{"glm":"glm-5.2","glm":"secret-model"}`},
		{name: "invalid provider", data: `{"secret/provider":"glm-5.2"}`},
		{name: "empty model", data: `{"glm":""}`},
		{name: "space in model", data: `{"glm":"secret model"}`},
		{name: "control in model", data: `{"glm":"secret\nmodel"}`},
		{name: "invalid utf8 model", data: "{\"glm\":\"\xff\"}"},
		{name: "oversized model", data: `{"glm":"` + strings.Repeat("x", 257) + `"}`},
		{name: "non-string model", data: `{"glm":52}`},
		{name: "trailing json", data: `{"glm":"glm-5.2"} {"secret":"model"}`},
		{name: "oversized file", data: `{"glm":"glm-5.2"}` + strings.Repeat(" ", 17<<10)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			writeModels(t, directory, tt.data)
			_, err := Load(directory)
			if err == nil {
				t.Fatal("Load() error = nil")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "glm-5.2") {
				t.Fatalf("Load() leaked catalog contents: %v", err)
			}
		})
	}
}

func TestLoadRejectsSymlinkAndNonRegularCatalog(t *testing.T) {
	t.Parallel()

	symlinkDir := t.TempDir()
	target := filepath.Join(symlinkDir, "target.json")
	if err := os.WriteFile(target, []byte(`{"glm":"glm-5.2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(symlinkDir, "models.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(symlinkDir); err == nil {
		t.Fatal("Load(symlink) error = nil")
	}

	directoryEntry := t.TempDir()
	if err := os.Mkdir(filepath.Join(directoryEntry, "models.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(directoryEntry); err == nil {
		t.Fatal("Load(directory) error = nil")
	}
}

func TestValidModel(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"glm-5.2", "anthropic/claude-4.1", "vendor:model_v2"} {
		if !ValidModel(value) {
			t.Errorf("ValidModel(%q) = false", value)
		}
	}
	for _, value := range []string{"", "glm 5.2", "glm\t5.2", "glm\n5.2", strings.Repeat("x", 257), string([]byte{0xff})} {
		if ValidModel(value) {
			t.Errorf("ValidModel(%q) = true", value)
		}
	}
}

func writeModels(t *testing.T, directory, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "models.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
