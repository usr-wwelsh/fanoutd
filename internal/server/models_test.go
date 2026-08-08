package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"fanoutd/internal/agent"
	"fanoutd/internal/config"
	"fanoutd/internal/llm"
	"fanoutd/internal/store"
)

type stubCatalog struct {
	list llm.ModelList
	err  error
}

func (s stubCatalog) ListModels(context.Context) (llm.ModelList, error) { return s.list, s.err }

func modelsServer(t *testing.T, c llm.Catalog) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	loop := agent.NewLoop(s, nil, filepath.Join(dir, "output"))
	ui := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html>")}}
	return New(s, loop, c, config.Config{}, ui)
}

// The picker decides between a dropdown and a text field on `kind`, so it has to
// survive the endpoint. A response that carried only the ids would leave the UI
// asserting tool support nobody claimed.
func TestModelsEndpointCarriesTheCatalogKind(t *testing.T) {
	srv := modelsServer(t, stubCatalog{list: llm.ModelList{
		Provider: "ollama",
		Kind:     llm.CatalogBare,
		Default:  "qwen3",
		Models:   []llm.Model{{ID: "qwen3", Name: "qwen3"}},
	}})

	resp := send(t, srv, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var got struct {
		Provider string `json:"provider"`
		Kind     string `json:"kind"`
		Default  string `json:"default"`
		Models   []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != "bare" {
		t.Errorf("kind = %q, want bare", got.Kind)
	}
	if got.Provider != "ollama" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Default != "qwen3" {
		t.Errorf("default = %q", got.Default)
	}
	if len(got.Models) != 1 {
		t.Errorf("got %d models, want 1", len(got.Models))
	}
}

// A provider with no catalog is a 200 with an empty list, not a 502: the board
// still works, and the model id becomes something the operator types.
func TestModelsEndpointServesAnEmptyCatalog(t *testing.T) {
	srv := modelsServer(t, stubCatalog{list: llm.ModelList{
		Provider: "llamacpp",
		Kind:     llm.CatalogNone,
		Default:  "local-model",
	}})

	resp := send(t, srv, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a provider without a catalog", resp.StatusCode)
	}
}
