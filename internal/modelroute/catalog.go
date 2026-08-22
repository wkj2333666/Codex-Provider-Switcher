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

type providerModels struct {
	defaultModel string
	allowed      map[string]struct{}
}

// Catalog is an immutable provider-to-model mapping.
type Catalog struct {
	providers map[string]providerModels
}

// Load reads the optional strict model catalog from a switcher state directory.
func Load(stateDirectory string) (*Catalog, error) {
	if stateDirectory == "" {
		return nil, errors.New("model route state directory is required")
	}
	path := filepath.Join(stateDirectory, catalogFileName)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return &Catalog{providers: map[string]providerModels{}}, nil
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
	providers := make(map[string]providerModels)
	for decoder.More() {
		rawProvider, err := decoder.Token()
		provider, ok := rawProvider.(string)
		if err != nil || !ok || !providerid.Valid(provider) {
			return nil, errors.New("invalid model route catalog")
		}
		if _, duplicate := providers[provider]; duplicate {
			return nil, errors.New("invalid model route catalog")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("invalid model route catalog")
		}
		models, err := decodeProviderModels(value)
		if err != nil {
			return nil, errors.New("invalid model route catalog")
		}
		providers[provider] = models
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, errors.New("invalid model route catalog")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid model route catalog")
	}
	return &Catalog{providers: providers}, nil
}

func decodeProviderModels(raw json.RawMessage) (providerModels, error) {
	var legacy string
	if json.Unmarshal(raw, &legacy) == nil {
		if !ValidModel(legacy) {
			return providerModels{}, errors.New("invalid model")
		}
		return providerModels{defaultModel: legacy, allowed: map[string]struct{}{legacy: {}}}, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return providerModels{}, errors.New("invalid provider models")
	}
	var defaultModel string
	var allowed map[string]struct{}
	seen := make(map[string]struct{})
	for decoder.More() {
		rawKey, err := decoder.Token()
		key, ok := rawKey.(string)
		if err != nil || !ok {
			return providerModels{}, errors.New("invalid provider models")
		}
		if _, duplicate := seen[key]; duplicate {
			return providerModels{}, errors.New("invalid provider models")
		}
		seen[key] = struct{}{}
		switch key {
		case "default":
			if decoder.Decode(&defaultModel) != nil || !ValidModel(defaultModel) {
				return providerModels{}, errors.New("invalid provider models")
			}
		case "models":
			models, err := decodeModelList(decoder)
			if err != nil {
				return providerModels{}, err
			}
			allowed = models
		default:
			return providerModels{}, errors.New("invalid provider models")
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF {
		return providerModels{}, errors.New("invalid provider models")
	}
	if defaultModel == "" || len(allowed) == 0 {
		return providerModels{}, errors.New("invalid provider models")
	}
	if _, ok := allowed[defaultModel]; !ok {
		return providerModels{}, errors.New("invalid provider models")
	}
	return providerModels{defaultModel: defaultModel, allowed: allowed}, nil
}

func decodeModelList(decoder *json.Decoder) (map[string]struct{}, error) {
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('[') {
		return nil, errors.New("invalid provider models")
	}
	models := make(map[string]struct{})
	for decoder.More() {
		var model string
		if decoder.Decode(&model) != nil || !ValidModel(model) {
			return nil, errors.New("invalid provider models")
		}
		if _, duplicate := models[model]; duplicate {
			return nil, errors.New("invalid provider models")
		}
		models[model] = struct{}{}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim(']') {
		return nil, errors.New("invalid provider models")
	}
	return models, nil
}

// Resolve returns the provider with its configured model when one exists.
func (catalog *Catalog) Resolve(provider string) Route {
	if provider == "" {
		return Route{}
	}
	route := Route{Provider: provider}
	if catalog != nil {
		route.Model = catalog.providers[provider].defaultModel
	}
	return route
}

// ResolveModel returns an exact provider/model route only when that model is
// allowlisted for the provider.
func (catalog *Catalog) ResolveModel(provider, model string) (Route, bool) {
	if catalog == nil || provider == "" || !ValidModel(model) {
		return Route{}, false
	}
	configured, ok := catalog.providers[provider]
	if !ok {
		return Route{}, false
	}
	if _, ok := configured.allowed[model]; !ok {
		return Route{}, false
	}
	return Route{Provider: provider, Model: model}, true
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
