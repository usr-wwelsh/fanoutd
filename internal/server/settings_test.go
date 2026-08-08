package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"fanoutd/internal/agent"
	"fanoutd/internal/config"
	"fanoutd/internal/llm"
	"fanoutd/internal/store"
)

// The settings page is the one place the API key and the board's own token are
// editable over HTTP, so these tests are as much about what the endpoint refuses
// as about what it saves.

type settingsFixture struct {
	srv  *Server
	loop *agent.Loop
	path string
}

// settingsServer builds a server whose settings file is a temporary one, so a
// test can read back exactly what was written without touching the developer's
// own .env.
func settingsServer(t *testing.T, contents string) *settingsFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FANOUT_ENV_FILE", path)
	// The settings file is the only source these tests want; an exported
	// variable from the developer's shell would outrule it and change what is
	// being asserted.
	for _, f := range settingFields {
		t.Setenv(f.Key, "")
		for _, legacy := range config.LegacyNames(f.Key) {
			t.Setenv(legacy, "")
		}
	}

	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	loop := agent.NewLoop(s, nil, filepath.Join(dir, "output"))
	ui := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html>")}}
	srv := New(s, loop, stubCatalog{}, config.Load(), ui)
	return &settingsFixture{srv: srv, loop: loop, path: path}
}

func (f *settingsFixture) file(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *settingsFixture) save(t *testing.T, values map[string]string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"values": values})
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	return send(t, f.srv, req)
}

type settingsResponse struct {
	File           string   `json:"file"`
	RestartPending []string `json:"restart_pending"`
	Warnings       []string `json:"warnings"`
	Fields         []struct {
		Key         string `json:"key"`
		Kind        string `json:"kind"`
		Value       string `json:"value"`
		Source      string `json:"source"`
		Set         bool   `json:"set"`
		Hint        string `json:"hint"`
		LegacyKey   string `json:"legacy_key"`
		Placeholder string `json:"placeholder"`
		Restart     bool   `json:"restart"`
	} `json:"fields"`
}

func (r settingsResponse) field(key string) (int, bool) {
	for i, f := range r.Fields {
		if f.Key == key {
			return i, true
		}
	}
	return 0, false
}

func decodeSettings(t *testing.T, resp *http.Response) settingsResponse {
	t.Helper()
	var got settingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func getSettings(t *testing.T, f *settingsFixture) settingsResponse {
	t.Helper()
	resp := send(t, f.srv, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/settings = %d", resp.StatusCode)
	}
	return decodeSettings(t, resp)
}

// Until now the API key existed only in a file and a process. Making settings
// editable puts it one GET away from anything that can reach the board, so the
// endpoint has to hand back the shape of the setting and never the secret.
func TestSecretsAreNeverServedBack(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-super-secret-value\nFANOUT_TOKEN=board-token\n")

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Header.Set("Authorization", "Bearer board-token")
	resp := send(t, f.srv, req)
	body := readBody(t, resp)
	if strings.Contains(body, "sk-super-secret-value") {
		t.Error("the API key was served in full")
	}
	if strings.Contains(body, "board-token") {
		t.Error("the board token was served in full")
	}

	var got settingsResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	key, ok := got.field("FANOUT_API_KEY")
	if !ok {
		t.Fatal("FANOUT_API_KEY is not in the settings")
	}
	if !got.Fields[key].Set || got.Fields[key].Value != "" {
		t.Errorf("api key field = %+v, want set with an empty value", got.Fields[key])
	}
	token, _ := got.field("FANOUT_TOKEN")
	if !got.Fields[token].Set {
		t.Error("the token does not report itself as set")
	}
	// A hint on the key identifies which one is loaded; the token is itself the
	// credential to this board, so no part of it is worth showing.
	if got.Fields[token].Hint != "" {
		t.Errorf("the token carries a hint: %q", got.Fields[token].Hint)
	}
}

// A settings page that requires re-typing the API key to change the step limit
// would have operators pasting their key into a form every time, which is worse
// than the file it replaced.
func TestASecretLeftOutOfTheRequestIsUnchanged(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-original\nFANOUT_MODEL=vendor/one\n")

	resp := f.save(t, map[string]string{"FANOUT_MODEL": "vendor/two"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}

	if !strings.Contains(f.file(t), "FANOUT_API_KEY=sk-original") {
		t.Errorf("the key did not survive an unrelated save:\n%s", f.file(t))
	}
	if !strings.Contains(f.file(t), "FANOUT_MODEL=vendor/two") {
		t.Errorf("the model was not saved:\n%s", f.file(t))
	}
}

// Sending the key with an empty value is how it is cleared, and it has to be
// distinguishable from not sending it at all.
func TestSendingASecretEmptyClearsIt(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-original\nFANOUT_PROVIDER=ollama\nFANOUT_MODEL=qwen3\n")

	resp := f.save(t, map[string]string{"FANOUT_API_KEY": ""})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if strings.Contains(f.file(t), "sk-original") {
		t.Errorf("the cleared key survived:\n%s", f.file(t))
	}
}

func TestAnUnknownKeyIsRefusedAndNothingIsWritten(t *testing.T) {
	f := settingsServer(t, "FANOUT_MODEL=vendor/one\n")
	before := f.file(t)

	resp := f.save(t, map[string]string{"PATH": "/tmp/evil:/usr/bin"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if f.file(t) != before {
		t.Errorf("the file changed despite the refusal:\n%s", f.file(t))
	}
}

func TestAValueWithALineBreakIsRefused(t *testing.T) {
	f := settingsServer(t, "FANOUT_MODEL=vendor/one\n")
	before := f.file(t)

	resp := f.save(t, map[string]string{"FANOUT_MODEL": "vendor/two\nFANOUT_SHELL=1"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if f.file(t) != before {
		t.Errorf("the file changed despite the refusal:\n%s", f.file(t))
	}
}

// Saving a provider the server then cannot build would leave the settings file
// describing a server that will not start. The resolve happens before the write
// so a rejected form leaves everything exactly as it was.
func TestAProviderThatWillNotResolveIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	f := settingsServer(t, "FANOUT_PROVIDER=openrouter\nFANOUT_API_KEY=sk-fine\nFANOUT_MODEL=vendor/one\n")
	before := f.file(t)

	// "custom" has no endpoint of its own, so this cannot be resolved.
	resp := f.save(t, map[string]string{"FANOUT_PROVIDER": "custom", "FANOUT_BASE_URL": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readBody(t, resp))
	}
	if f.file(t) != before {
		t.Errorf("the file changed despite the refusal:\n%s", f.file(t))
	}
}

func TestAnUnknownProviderIsRefused(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-fine\n")
	resp := f.save(t, map[string]string{"FANOUT_PROVIDER": "not-a-provider"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestABaseUrlMustBeAnHttpEndpoint(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-fine\n")
	for _, bad := range []string{"file:///etc/passwd", "not a url at all", "ftp://example.com"} {
		resp := f.save(t, map[string]string{"FANOUT_BASE_URL": bad})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("base url %q gave %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestNumericSettingsRejectRubbish(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-fine\n")
	resp := f.save(t, map[string]string{"FANOUT_MAX_STEPS": "plenty"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// The whole point of the page: a saved setting is in force immediately. Review
// is the one visible through an endpoint of its own.
func TestReviewTakesEffectWithoutARestart(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-fine\n")

	if boardConfig(t, f)["review"] != false {
		t.Fatal("review is already on before the test starts")
	}

	resp := f.save(t, map[string]string{"FANOUT_REVIEW": "1", "FANOUT_REVIEW_MODEL": "vendor/judge"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}

	cfg := boardConfig(t, f)
	if cfg["review"] != true {
		t.Error("/api/config still reports review off")
	}
	if cfg["review_model"] != "vendor/judge" {
		t.Errorf("review_model = %v", cfg["review_model"])
	}
}

// The provider is what the running loop and the model picker both hang off, so
// a change has to reach the client rather than only the file.
func TestChangingTheProviderSwapsTheLiveClient(t *testing.T) {
	f := settingsServer(t, "FANOUT_PROVIDER=openrouter\nFANOUT_API_KEY=sk-fine\nFANOUT_MODEL=vendor/one\n")

	resp := f.save(t, map[string]string{"FANOUT_PROVIDER": "ollama", "FANOUT_MODEL": "qwen3"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}

	// The catalog the models endpoint answers from is now the new provider's,
	// and it names itself in the response.
	models := send(t, f.srv, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	var list llm.ModelList
	if err := json.NewDecoder(models.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Provider != "ollama" {
		t.Errorf("provider = %q, want ollama", list.Provider)
	}
	if list.Default != "qwen3" {
		t.Errorf("default model = %q, want qwen3", list.Default)
	}
}

// Turning the token off has to take the gate off the running server, and turning
// it on has to put one there — otherwise the setting is a lie until a restart,
// and it is the one setting where believing it is a security decision.
func TestSettingTheTokenGatesTheRunningServer(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-fine\n")

	open := send(t, f.srv, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if open.StatusCode != http.StatusOK {
		t.Fatalf("the board was not open to start with: %d", open.StatusCode)
	}

	if resp := f.save(t, map[string]string{"FANOUT_TOKEN": "now-gated"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}

	closed := send(t, f.srv, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if closed.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the board is still open after a token was set: %d", closed.StatusCode)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer now-gated")
	if allowed := send(t, f.srv, req); allowed.StatusCode != http.StatusOK {
		t.Fatalf("the new token was refused: %d", allowed.StatusCode)
	}
}

// Settings are as gated as the rest of the API. They are the most sensitive
// thing behind it, so a route that reached them without a token would hand over
// the shell settings and the board's own credential.
func TestSettingsAreGatedByTheToken(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-fine\nFANOUT_TOKEN=needed\n")

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/settings", nil),
		httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"values":{}}`)),
	} {
		if resp := send(t, f.srv, req); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s /api/settings without a token = %d, want 401", req.Method, resp.StatusCode)
		}
	}
}

// A port change cannot be adopted by a listener already bound, so it is saved
// and reported rather than silently ignored.
func TestAPortChangeIsSavedAndReportedAsNeedingARestart(t *testing.T) {
	f := settingsServer(t, "FANOUT_API_KEY=sk-fine\nPORT=8080\n")

	resp := f.save(t, map[string]string{"PORT": "9091"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	got := decodeSettings(t, resp)
	if !contains(got.RestartPending, "PORT") {
		t.Errorf("restart_pending = %v, want it to name PORT", got.RestartPending)
	}
	if !strings.Contains(f.file(t), "PORT=9091") {
		t.Errorf("the port was not saved:\n%s", f.file(t))
	}

	// And it stays reported, so the banner survives a page reload rather than
	// existing only in the response to the save.
	if again := getSettings(t, f); !contains(again.RestartPending, "PORT") {
		t.Errorf("restart_pending = %v on a fresh read", again.RestartPending)
	}
}

// A setting exported into the environment beats the file both now and at the
// next start. Saving it anyway and saying nothing would be a form that appears
// to work and does not.
func TestAnExportedSettingIsReportedAsComingFromTheEnvironment(t *testing.T) {
	f := settingsServer(t, "FANOUT_MODEL=from/file\n")
	t.Setenv("FANOUT_MODEL", "from/env")

	got := getSettings(t, f)
	i, ok := got.field("FANOUT_MODEL")
	if !ok {
		t.Fatal("FANOUT_MODEL is missing")
	}
	if got.Fields[i].Source != "env" {
		t.Errorf("source = %q, want env", got.Fields[i].Source)
	}
	if got.Fields[i].Value != "from/env" {
		t.Errorf("value = %q, want the one actually in force", got.Fields[i].Value)
	}
}

// Superseded names have to go when the current one is written, or the file ends
// up holding two API keys and only one of them is in use.
func TestWritingASettingDropsTheNameItSuperseded(t *testing.T) {
	f := settingsServer(t, "OPENROUTER_API_KEY=sk-legacy\nFANOUT_MODEL=vendor/one\n")

	resp := f.save(t, map[string]string{"FANOUT_API_KEY": "sk-current"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if strings.Contains(f.file(t), "sk-legacy") {
		t.Errorf("the superseded key is still in the file:\n%s", f.file(t))
	}
}

// Asking for a shell on a machine with no bubblewrap saves the setting — it is
// the operator's intent, and it will hold on a machine that has it — but the
// answer has to say the tool is not actually on, exactly as startup does.
func TestAShellThatWillNotStartIsSavedWithAWarning(t *testing.T) {
	if _, err := agent.NewSandbox(agent.SandboxConfig{StateDir: t.TempDir()}); err == nil {
		t.Skip("bubblewrap works here, so there is no failure to report")
	}
	f := settingsServer(t, "FANOUT_API_KEY=sk-fine\n")

	resp := f.save(t, map[string]string{"FANOUT_SHELL": "1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	got := decodeSettings(t, resp)
	if len(got.Warnings) == 0 {
		t.Error("no warning that the shell did not come up")
	}
	if boardConfig(t, f)["shell"] != false {
		t.Error("/api/config claims a shell the sandbox could not provide")
	}
}

// The shell is the setting with a jail behind it rather than a variable, so
// turning it on has to build one and turning it off has to take it away — both
// without a restart, and both visible where the board asks whether agents have
// a shell.
func TestTheShellIsBuiltAndTornDownLive(t *testing.T) {
	if _, err := agent.NewSandbox(agent.SandboxConfig{StateDir: t.TempDir()}); err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
	f := settingsServer(t, "FANOUT_API_KEY=sk-fine\n")

	if boardConfig(t, f)["shell"] != false {
		t.Fatal("agents already have a shell before it was asked for")
	}

	if resp := f.save(t, map[string]string{"FANOUT_SHELL": "1"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if boardConfig(t, f)["shell"] != true {
		t.Error("the shell was saved but agents still do not have one")
	}

	if resp := f.save(t, map[string]string{"FANOUT_SHELL": "0"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if boardConfig(t, f)["shell"] != false {
		t.Error("the shell was switched off and agents still have one")
	}
}

// A board running on OPENROUTER_API_KEY has a key. Reporting it as unset told
// the operator their key was missing while the board was happily using it, and
// would have had the page propose a config with no key in it at all.
func TestASettingInForceUnderItsOldNameReadsAsSet(t *testing.T) {
	f := settingsServer(t, "OPENROUTER_API_KEY=sk-legacy-value\nOPENROUTER_MODEL=vendor/legacy\n")

	got := getSettings(t, f)

	key, _ := got.field("FANOUT_API_KEY")
	if !got.Fields[key].Set {
		t.Error("the key the server is running on reads as unset")
	}
	if got.Fields[key].LegacyKey != "OPENROUTER_API_KEY" {
		t.Errorf("legacy_key = %q, want the name it is actually set under", got.Fields[key].LegacyKey)
	}
	model, _ := got.field("FANOUT_MODEL")
	if got.Fields[model].Value != "vendor/legacy" {
		t.Errorf("model = %q, want the one in force", got.Fields[model].Value)
	}
}

// An empty model field is not "no model" — it is the provider's own default, and
// which one that is depends entirely on the provider.
func TestAnEmptyModelOffersTheProvidersOwnDefault(t *testing.T) {
	f := settingsServer(t, "FANOUT_PROVIDER=openrouter\nFANOUT_API_KEY=sk-fine\n")

	got := getSettings(t, f)
	i, _ := got.field("FANOUT_MODEL")
	if got.Fields[i].Placeholder != "inclusionai/ling-3.0-flash:free" {
		t.Errorf("placeholder = %q, want openrouter's default model", got.Fields[i].Placeholder)
	}
	base, _ := got.field("FANOUT_BASE_URL")
	if got.Fields[base].Placeholder != "https://openrouter.ai/api/v1" {
		t.Errorf("base url placeholder = %q, want openrouter's endpoint", got.Fields[base].Placeholder)
	}
}

// A provider with no default has to say so rather than showing another
// provider's model as if it applied.
func TestAProviderWithNoDefaultModelSaysTheFieldIsRequired(t *testing.T) {
	f := settingsServer(t, "FANOUT_PROVIDER=openai\nFANOUT_API_KEY=sk-fine\nFANOUT_MODEL=gpt-5\n")

	got := getSettings(t, f)
	i, _ := got.field("FANOUT_MODEL")
	if !strings.Contains(got.Fields[i].Placeholder, "no default") {
		t.Errorf("placeholder = %q, want it to say the provider has none", got.Fields[i].Placeholder)
	}
}

func TestSettingsRejectOtherMethods(t *testing.T) {
	f := settingsServer(t, "")
	resp := send(t, f.srv, httptest.NewRequest(http.MethodDelete, "/api/settings", nil))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", resp.StatusCode)
	}
}

func boardConfig(t *testing.T, f *settingsFixture) map[string]any {
	t.Helper()
	resp := send(t, f.srv, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
