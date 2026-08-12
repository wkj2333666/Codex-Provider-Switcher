// Package modelroute resolves optional provider-specific model ids.
package modelroute

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode"
	"unicode/utf8"

	providerid "github.com/wkj2333666/Codex-Provider-Switcher/internal/provider"
)

const (
	catalogFileName = "models.json"
	maxCatalogSize  = 16 << 10
	maxModelSize    = 256
)

// Route identifies one provider and its optional configured model.
type Route struct {
	Provider string
	Model    string
}

// Catalog is an immutable provider-to-model mapping.
type Catalog struct {
	models map[string]string
}

// Load reads the optional strict model catalog from a switcher state directory.
func Load(stateDirectory string) (*Catalog, error) {
	if stateDirectory == "" {
		return nil, errors.New("model route state directory is required")
	}
	path := filepath.Join(stateDirectory, catalogFileName)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return &Catalog{models: map[string]string{}}, nil
	}
	if err != nil {
		return nil, errors.New("open model route catalog")
	}
	defer file.Close()
	return readCatalog(file)
}

func readCatalog(file *os.File) (*Catalog, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxCatalogSize {
		return nil, errors.New("invalid model route catalog file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCatalogSize+1))
	if err != nil || len(data) > maxCatalogSize {
		return nil, errors.New("read model route catalog")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("invalid model route catalog")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("invalid model route catalog")
	}
	models := make(map[string]string)
	for decoder.More() {
		rawProvider, err := decoder.Token()
		provider, ok := rawProvider.(string)
		if err != nil || !ok || !providerid.Valid(provider) {
			return nil, errors.New("invalid model route catalog")
		}
		if _, duplicate := models[provider]; duplicate {
			return nil, errors.New("invalid model route catalog")
		}
		var model string
		if decoder.Decode(&model) != nil || !ValidModel(model) {
			return nil, errors.New("invalid model route catalog")
		}
		models[provider] = model
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, errors.New("invalid model route catalog")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid model route catalog")
	}
	return &Catalog{models: models}, nil
}

// Resolve returns the provider with its configured model when one exists.
func (catalog *Catalog) Resolve(provider string) Route {
	if provider == "" {
		return Route{}
	}
	route := Route{Provider: provider}
	if catalog != nil {
		route.Model = catalog.models[provider]
	}
	return route
}

// ValidModel reports whether a model id is safe to persist and route.
func ValidModel(model string) bool {
	if model == "" || len(model) > maxModelSize || !utf8.ValidString(model) {
		return false
	}
	for _, character := range model {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}
