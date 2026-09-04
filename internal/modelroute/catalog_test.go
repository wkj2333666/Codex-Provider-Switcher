package modelroute

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestLoadResolvesAllowlistedProviderModels(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	writeModels(t, directory, `{
  "glm": {
    "default": "glm-5.3",
    "models": ["glm-5.3", "glm-5.3-flash", "glm-5.2"]
  }
}`)

	catalog, err := Load(directory)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := catalog.Resolve("glm"); got != (Route{Provider: "glm", Model: "glm-5.3"}) {
		t.Fatalf("Resolve(glm) = %#v, want default GLM route", got)
	}
	if got, ok := catalog.ResolveModel("glm", "glm-5.2"); !ok || got != (Route{Provider: "glm", Model: "glm-5.2"}) {
		t.Fatalf("ResolveModel(glm, glm-5.2) = %#v, %v", got, ok)
	}
	if got, ok := catalog.ResolveModel("glm", "glm-5.3-flash"); !ok || got != (Route{Provider: "glm", Model: "glm-5.3-flash"}) {
		t.Fatalf("ResolveModel(glm, glm-5.3-flash) = %#v, %v", got, ok)
	}
	if got, ok := catalog.ResolveModel("glm", "deepseek-v4-pro"); ok || got != (Route{}) {
		t.Fatalf("ResolveModel(glm, foreign model) = %#v, %v", got, ok)
	}
}

func TestLoadRejectsInvalidAllowlistedProviderModels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
	}{
		{name: "missing default", data: `{"glm":{"models":["glm-5.3"]}}`},
		{name: "missing models", data: `{"glm":{"default":"glm-5.3"}}`},
		{name: "default not allowed", data: `{"glm":{"default":"glm-5.3","models":["glm-5.2"]}}`},
		{name: "duplicate model", data: `{"glm":{"default":"glm-5.3","models":["glm-5.3","glm-5.3"]}}`},
		{name: "empty models", data: `{"glm":{"default":"glm-5.3","models":[]}}`},
		{name: "unknown field", data: `{"glm":{"default":"glm-5.3","models":["glm-5.3"],"fallback":"secret-model"}}`},
		{name: "duplicate default", data: `{"glm":{"default":"glm-5.3","default":"secret-model","models":["glm-5.3"]}}`},
		{name: "duplicate models field", data: `{"glm":{"default":"glm-5.3","models":["glm-5.3"],"models":["secret-model"]}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			writeModels(t, directory, tt.data)
			_, err := Load(directory)
			if err == nil {
				t.Fatal("Load() error = nil")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "glm-5") {
				t.Fatalf("Load() leaked catalog contents: %v", err)
			}
		})
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

func TestLoadRejectsFIFOWithoutBlocking(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "models.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Load(directory)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Load(FIFO) error = nil")
		}
	case <-time.After(time.Second):
		t.Fatal("Load(FIFO) blocked")
	}
}

func TestReadCatalogUsesOpenedDescriptorAfterPathReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "models.json")
	writeModels(t, directory, `{"glm":"glm-5.2"}`)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, filepath.Join(directory, "opened.json")); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(directory, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"glm":"secret-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, path); err != nil {
		t.Fatal(err)
	}

	catalog, err := readCatalog(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Resolve("glm"); got != (Route{Provider: "glm", Model: "glm-5.2"}) {
		t.Fatalf("Resolve(glm) = %#v, want original opened catalog", got)
	}
}

func TestReadCatalogRejectsClosedDescriptorWithStaticError(t *testing.T) {
	directory := t.TempDir()
	writeModels(t, directory, `{"glm":"secret-model"}`)
	file, err := os.Open(filepath.Join(directory, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readCatalog(file); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), directory) {
		t.Fatalf("readCatalog(closed) error = %v", err)
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
