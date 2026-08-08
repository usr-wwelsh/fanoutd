package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// openRouterCatalog is the shape OpenRouter publishes: pricing and per-model
// parameter support alongside the id.
const openRouterCatalog = `{"data":[
	{"id":"vendor/paid","name":"Paid","context_length":128000,
	 "pricing":{"prompt":"0.000001","completion":"0.000002"},
	 "supported_parameters":["tools"]},
	{"id":"vendor/free","name":"Free","context_length":32000,
	 "pricing":{"prompt":"0","completion":"0"},
	 "supported_parameters":["tools"]}
]}`

// bareCatalog is what the OpenAI /models schema actually specifies, and what
// almost every other provider returns.
const bareCatalog = `{"data":[
	{"id":"qwen3","object":"model","owned_by":"library"},
	{"id":"gemma3","object":"model","owned_by":"library"}
]}`

func catalogServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRichCatalogKeepsPricingAndToolSupport(t *testing.T) {
	srv := catalogServer(t, http.StatusOK, openRouterCatalog)
	c := NewClient(presets["openrouter"], "k", "m", srv.URL)

	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got.Kind != CatalogRich {
		t.Fatalf("kind = %q, want rich", got.Kind)
	}
	// Free first, which is what the picker groups on.
	if got.Models[0].ID != "vendor/free" {
		t.Errorf("first model = %q, want the free one", got.Models[0].ID)
	}
	if !got.Models[0].Free || !got.Models[0].Tools || got.Models[0].ContextLength != 32000 {
		t.Errorf("rich fields dropped: %+v", got.Models[0])
	}
}

// The bare response parses without error and simply has no pricing or parameter
// list in it. Reading those fields anyway would price every model at nothing and
// report that none of them call tools — both false, and both worse than silence.
func TestBareCatalogClaimsNothingItWasNotTold(t *testing.T) {
	srv := catalogServer(t, http.StatusOK, bareCatalog)
	c := NewClient(presets["ollama"], "", "qwen3", srv.URL)

	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got.Kind != CatalogBare {
		t.Fatalf("kind = %q, want bare", got.Kind)
	}
	if len(got.Models) != 2 {
		t.Fatalf("got %d models, want 2", len(got.Models))
	}
	for _, m := range got.Models {
		if m.Free || m.Tools || m.ContextLength != 0 {
			t.Errorf("bare entry asserts what nobody said: %+v", m)
		}
	}
	if got.Models[0].ID != "gemma3" {
		t.Errorf("bare catalog not sorted by id: %+v", got.Models)
	}
}

// A local server is entitled to implement no /models at all. That must leave the
// picker with a text field, not an error page — the operator knows the id.
func TestMissingBareCatalogDegradesRatherThanFails(t *testing.T) {
	srv := catalogServer(t, http.StatusNotFound, "not found")
	c := NewClient(presets["llamacpp"], "", "local-model", srv.URL)

	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("a provider without a catalog is not an error: %v", err)
	}
	if got.Kind != CatalogNone {
		t.Errorf("kind = %q, want none", got.Kind)
	}
	if len(got.Models) != 0 {
		t.Errorf("got %d models from a 404", len(got.Models))
	}
	if got.Default != "local-model" {
		t.Errorf("default = %q, want the configured model to survive", got.Default)
	}
}

// A provider that does publish a catalog failing to serve it is a real fault,
// and stays one: silently degrading it would hide an outage behind an empty
// picker.
func TestFailingRichCatalogIsStillAnError(t *testing.T) {
	srv := catalogServer(t, http.StatusInternalServerError, "boom")
	c := NewClient(presets["openrouter"], "k", "m", srv.URL)

	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("want an error when a rich catalog fails")
	}
}

func TestModelListNamesItsProviderAndDefault(t *testing.T) {
	srv := catalogServer(t, http.StatusOK, bareCatalog)
	c := NewClient(presets["groq"], "k", "llama-3.3-70b-versatile", srv.URL)

	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got.Provider != "groq" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Default != "llama-3.3-70b-versatile" {
		t.Errorf("default = %q", got.Default)
	}
}
