package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// modelsFixture serves a mutable OpenAI-style model list over the host
// HTTP client seam.
type modelsFixture struct {
	mu       sync.Mutex
	status   int
	body     string
	calls    int
	block    bool
	entered  chan struct{}
	sawDone  atomic.Bool
	lastAuth string
	lastReq  string
	custom   func(http.ResponseWriter, *http.Request)
	server   *httptest.Server
}

func newModelsFixture(t *testing.T) *modelsFixture {
	t.Helper()
	f := &modelsFixture{status: 200, body: `{"data":[]}`, entered: make(chan struct{}, 1)}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *modelsFixture) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	f.lastAuth = r.Header.Get("Authorization")
	f.lastReq = r.Method + " " + r.URL.Path
	status, body, block, custom := f.status, f.body, f.block, f.custom
	f.mu.Unlock()
	if custom != nil {
		custom(w, r)
		return
	}
	if block {
		select {
		case f.entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		f.sawDone.Store(true)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	_, _ = io.WriteString(w, body)
}

func (f *modelsFixture) set(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body, f.block = status, body, false
}

func (f *modelsFixture) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func modelsAuthReq(key string) pluginapi.AuthModelRequest {
	return pluginapi.AuthModelRequest{
		AuthID:       "auth-1",
		AuthProvider: provider,
		StorageJSON:  []byte(`{"api_key":"` + key + `"}`),
		Plugin:       pluginapi.Metadata{Name: provider},
	}
}

func modelIDs(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func equalIDs(got []pluginapi.ModelInfo, want ...string) bool {
	gotIDs := modelIDs(got)
	if len(gotIDs) != len(want) {
		return false
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			return false
		}
	}
	return true
}

func newTestCatalog() *kit.ModelCatalog {
	return newModelCatalog()
}

func discover(t *testing.T, c *kit.ModelCatalog, f *modelsFixture, key string) []pluginapi.ModelInfo {
	t.Helper()
	req := modelsAuthReq(key)
	req.HTTPClient = mockHostClient{f.server}
	resp, err := c.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Models
}

func TestModelCatalogParseCarriesWireFields(t *testing.T) {
	f := newModelsFixture(t)
	f.set(200, `{"data":[{"id":"deepseek-v4.1-flash","object":"model","created":123,"owned_by":"hopper-org"},{"id":"example/model"}]}`)
	c := newTestCatalog()
	models := discover(t, c, f, "example-key")
	if len(models) != 2 {
		t.Fatalf("models %v want 2", modelIDs(models))
	}
	if models[0].ID != "hopper/deepseek-v4.1-flash" || models[0].Created != 123 || models[0].OwnedBy != "hopper-org" || models[0].DisplayName != "deepseek-v4.1-flash" {
		t.Fatalf("wire fields lost: %+v", models[0])
	}
	// Missing wire fields fall back to the former catalog defaults.
	if models[1].ID != "hopper/example/model" || models[1].OwnedBy != provider || models[1].Object != "model" {
		t.Fatalf("defaults missing: %+v", models[1])
	}
	if f.lastAuth != "Bearer example-key" || f.lastReq != "GET /v1/models" {
		t.Fatalf("discovery request auth %q path %q want Bearer example-key GET /v1/models", f.lastAuth, f.lastReq)
	}
}

func TestModelCatalogStaticPipelineServesFallback(t *testing.T) {
	c := newModelCatalog()
	ctx := context.Background()
	static, err := c.StaticModels(ctx, pluginapi.StaticModelRequest{Plugin: pluginapi.Metadata{Name: provider}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := c.RegisterModels(ctx, pluginapi.ModelRegistrationRequest{Plugin: pluginapi.Metadata{Name: provider}})
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(static.Models, modelIDs(fallbackModels())...) || !equalIDs(reg.Models, modelIDs(fallbackModels())...) {
		t.Fatalf("static %v register %v want fallback %v", modelIDs(static.Models), modelIDs(reg.Models), modelIDs(fallbackModels()))
	}
	for _, m := range append(static.Models, reg.Models...) {
		if m.DisplayName == "" {
			t.Fatalf("static display name lost: %+v", m)
		}
	}
}

func TestModelCatalogFiltersEmbeddingsNormalizesAndDedupes(t *testing.T) {
	f := newModelsFixture(t)
	f.set(200, `{"data":[{"id":"nomic-embed-text-v1.5"},{"id":"EMBED-rerank-x"},{"id":"text-Embedding-3"},{"id":"  deepseek-v4.1-flash  "},{"id":""},{"id":"   "},{"id":"deepseek-v4.1-flash"}]}`)
	c := newTestCatalog()
	models := discover(t, c, f, "example-key")
	if !equalIDs(models, "hopper/deepseek-v4.1-flash") {
		t.Fatalf("models %v want only hopper/deepseek-v4.1-flash", modelIDs(models))
	}
}

func TestModelCatalogPrefixedServedOnceDoubledDropped(t *testing.T) {
	f := newModelsFixture(t)
	f.set(200, `{"data":[{"id":"hopper/already"},{"id":"hopper/hopper/doubled"},{"id":"plain-model"}]}`)
	c := newTestCatalog()
	models := discover(t, c, f, "example-key")
	// A single vendor prefix is served once; a doubled prefix is rejected by
	// the kit namespace filter instead of being served.
	if !equalIDs(models, "hopper/already", "hopper/plain-model") {
		t.Fatalf("models %v want hopper/already + hopper/plain-model", modelIDs(models))
	}
	// A bare id and its prefixed twin collide after namespacing: only one
	// is served. A lone bare "hopper" becomes "hopper/hopper" and is
	// rejected, so an all-rejected payload falls back to the static list.
	f.set(200, `{"data":[{"id":"twin"},{"id":"hopper/twin"}]}`)
	if models := discover(t, c, f, "example-key"); !equalIDs(models, "hopper/twin") {
		t.Fatalf("models %v want single hopper/twin", modelIDs(models))
	}
	f.set(200, `{"data":[{"id":"hopper"}]}`)
	if models := discover(t, newTestCatalog(), f, "example-key"); !equalIDs(models, modelIDs(fallbackModels())...) {
		t.Fatalf("models %v want static fallback for bare hopper", modelIDs(models))
	}
}

// The kit ModelCatalog has no per-call error surface for a fetch failure:
// a missing key or a dead host client is a failed fetch, and the catalog
// serves the fallback instead of an error. These tests pin that behaviour.
func TestModelCatalogMissingKeyOrClientServesFallback(t *testing.T) {
	f := newModelsFixture(t)
	c := newTestCatalog()
	broken := pluginapi.AuthModelRequest{
		AuthID:       "auth-1",
		AuthProvider: provider,
		StorageJSON:  []byte(`{"other":"value"}`),
		Plugin:       pluginapi.Metadata{Name: provider},
		HTTPClient:   mockHostClient{f.server},
	}
	resp, err := c.ModelsForAuth(context.Background(), broken)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(resp.Models, modelIDs(fallbackModels())...) {
		t.Fatalf("models %v want static fallback for missing key", modelIDs(resp.Models))
	}
	// No HTTP client at all: same fallback, same zero error.
	broken.HTTPClient = nil
	resp, err = c.ModelsForAuth(context.Background(), broken)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(resp.Models, modelIDs(fallbackModels())...) {
		t.Fatalf("models %v want static fallback without client", modelIDs(resp.Models))
	}
}

func TestModelCatalogOverridesWithExecutorMapping(t *testing.T) {
	f := newModelsFixture(t)
	f.set(200, `{"data":[{"id":"known"}]}`)
	cfg := modelCatalogConfig()
	cfg.Overrides = map[string]kit.ModelOverride{
		"known": {DisplayName: "Known", ContextLength: 100, MaxCompletionTokens: 10, ContributorID: "known-contributor"},
	}
	c := kit.NewModelCatalog(cfg)
	models := discover(t, c, f, "example-key")
	if len(models) != 1 {
		t.Fatalf("models %v want 1", modelIDs(models))
	}
	m := models[0]
	if m.ID != "hopper/known-contributor" || m.DisplayName != "Known" || m.ContextLength != 100 || m.MaxCompletionTokens != 10 {
		t.Fatalf("override lost: %+v", m)
	}
	// Executor mapping: the emitted id strips exactly one namespace before
	// the upstream request.
	payload, _ := json.Marshal(map[string]any{"model": m.ID, "messages": []any{}})
	req, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: payload, StorageJSON: []byte(`{"api_key":"example-key"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil || body["model"] != "known-contributor" {
		t.Fatalf("executor upstream model %s want known-contributor", req.Body)
	}
}

func TestModelCatalogContributorSiblingsWithoutFixtures(t *testing.T) {
	f := newModelsFixture(t)
	f.set(200, `{"data":[{"id":"unknown-a"},{"id":"unknown-a-contributor"},{"id":"unknown-solo"}]}`)
	c := newTestCatalog()
	models := discover(t, c, f, "example-key")
	// No per-model fixture for these previously unknown ids: the contributor
	// sibling wins, a solo id keeps its base id.
	if !equalIDs(models, "hopper/unknown-a-contributor", "hopper/unknown-solo") {
		t.Fatalf("models %v want contributor win + kept solo", modelIDs(models))
	}
	kit.AssertModelNamespace(t, provider, models)
}

// Timeline A: every invocation fetches, so a catalog change converges at
// the next host event.
func TestModelCatalogTimelineAFreshConvergence(t *testing.T) {
	f := newModelsFixture(t)
	f.set(200, `{"data":[{"id":"a-model"}]}`)
	c := newTestCatalog()
	if models := discover(t, c, f, "example-key"); !equalIDs(models, "hopper/a-model") {
		t.Fatalf("first models %v", modelIDs(models))
	}
	f.set(200, `{"data":[{"id":"a-model"},{"id":"b-model"}]}`)
	if models := discover(t, c, f, "example-key"); !equalIDs(models, "hopper/a-model", "hopper/b-model") {
		t.Fatalf("second models %v want converged fresh list", modelIDs(models))
	}
	if n := f.callCount(); n != 2 {
		t.Fatalf("fetches %d want 2 (every invocation fetches)", n)
	}
	// A TTL never skips a fetch either.
	withTTL := kit.NewModelCatalogWithTTL(modelCatalogConfig(), time.Hour)
	discover(t, withTTL, f, "example-key")
	before := f.callCount()
	discover(t, withTTL, f, "example-key")
	if n := f.callCount(); n != before+1 {
		t.Fatalf("TTL catalog fetches %d want %d", n, before+1)
	}
}

// Timeline B is documented, never claimed as a guarantee: a transient
// failure serves last-good and the kit performs a single fetch per
// ModelsForAuth invocation, so no background recovery is promised.
func TestModelCatalogTimelineBFailureThenNoGuaranteedRecovery(t *testing.T) {
	f := newModelsFixture(t)
	f.set(200, `{"data":[{"id":"a-model"}]}`)
	c := newTestCatalog()
	discover(t, c, f, "example-key")
	f.set(500, `{"data":[{"id":"a-model"},{"id":"b-model"}]}`)
	models := discover(t, c, f, "example-key")
	if !equalIDs(models, "hopper/a-model") {
		t.Fatalf("models %v want last-good hopper/a-model (b-model unpublished)", modelIDs(models))
	}
	before := f.callCount()
	time.Sleep(60 * time.Millisecond)
	if n := f.callCount(); n != before {
		t.Fatalf("fetches %d want %d: no background fetch without a host callback", n, before)
	}
}

func TestModelCatalogLastGoodVersusStaticFallback(t *testing.T) {
	f := newModelsFixture(t)
	c := newTestCatalog()
	// No last-good yet: a fetch failure serves the static fallback.
	f.set(500, `{"error":"down"}`)
	if models := discover(t, c, f, "example-key"); !equalIDs(models, modelIDs(fallbackModels())...) {
		t.Fatalf("models %v want static fallback", modelIDs(models))
	}
	// A success replaces the fallback with fresh models.
	f.set(200, `{"data":[{"id":"fresh-model"}]}`)
	if models := discover(t, c, f, "example-key"); !equalIDs(models, "hopper/fresh-model") {
		t.Fatalf("models %v want fresh", modelIDs(models))
	}
	// A later failure serves last-good, not static.
	f.set(500, `{"error":"down"}`)
	if models := discover(t, c, f, "example-key"); !equalIDs(models, "hopper/fresh-model") {
		t.Fatalf("models %v want last-good", modelIDs(models))
	}
	// An empty upstream list is a failed fetch, not an intentional empty.
	f.set(200, `{"data":[]}`)
	if models := discover(t, c, f, "example-key"); !equalIDs(models, "hopper/fresh-model") {
		t.Fatalf("models %v want last-good after empty upstream", modelIDs(models))
	}
	// A filtered-empty result is a legitimate policy outcome: it is served
	// and cached, so a later failure keeps serving empty.
	f.set(200, `{"data":[{"id":"nomic-embed-text-v1.5"}]}`)
	if models := discover(t, c, f, "example-key"); len(models) != 0 {
		t.Fatalf("models %v want filtered-empty served", modelIDs(models))
	}
	f.set(500, `{"error":"down"}`)
	if models := discover(t, c, f, "example-key"); len(models) != 0 {
		t.Fatalf("models %v want cached filtered-empty after failure", modelIDs(models))
	}
}

func TestModelCatalogCredentialGenerationIsolation(t *testing.T) {
	f := newModelsFixture(t)
	c := newTestCatalog()
	f.set(200, `{"data":[{"id":"a-model"}]}`)
	discover(t, c, f, "key-a")
	// A rotated credential under the same auth identity must never receive
	// the old generation's list: a failing fetch falls back to static.
	f.set(500, `{"error":"down"}`)
	rotated := modelsAuthReq("key-b")
	rotated.HTTPClient = mockHostClient{f.server}
	resp, err := c.ModelsForAuth(context.Background(), rotated)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(resp.Models, modelIDs(fallbackModels())...) {
		t.Fatalf("models %v want static for rotated credential", modelIDs(resp.Models))
	}
	// The original generation still gets its own last-good.
	if models := discover(t, c, f, "key-a"); !equalIDs(models, "hopper/a-model") {
		t.Fatalf("models %v want generation A last-good", modelIDs(models))
	}
}

func TestModelCatalogPerKeyEntitlement(t *testing.T) {
	f := newModelsFixture(t)
	f.mu.Lock()
	f.custom = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer key-one" {
			_, _ = io.WriteString(w, `{"data":[{"id":"model-one"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"model-two"}]}`)
	}
	f.mu.Unlock()
	c := newTestCatalog()
	if models := discover(t, c, f, "key-one"); !equalIDs(models, "hopper/model-one") {
		t.Fatalf("models %v want key-one list", modelIDs(models))
	}
	if models := discover(t, c, f, "key-two"); !equalIDs(models, "hopper/model-two") {
		t.Fatalf("models %v want key-two list", modelIDs(models))
	}
	if n := f.callCount(); n != 2 {
		t.Fatalf("fetches %d want 2 (per-key discovery)", n)
	}
}

func TestModelCatalogOversizedBodyFallsBack(t *testing.T) {
	f := newModelsFixture(t)
	f.set(200, strings.Repeat("x", kit.MaxModelsBody+1))
	c := newTestCatalog()
	// Fails before any success, so the static fallback serves.
	if models := discover(t, c, f, "example-key"); !equalIDs(models, modelIDs(fallbackModels())...) {
		t.Fatalf("models %v want static fallback for oversized body", modelIDs(models))
	}
	if n := f.callCount(); n != 1 {
		t.Fatalf("fetches %d want 1", n)
	}
}

func TestModelCatalogCancellationFallsBack(t *testing.T) {
	f := newModelsFixture(t)
	f.mu.Lock()
	f.block = true
	f.mu.Unlock()
	c := newTestCatalog()
	ctx, cancel := context.WithCancel(context.Background())
	req := modelsAuthReq("example-key")
	req.HTTPClient = mockHostClient{f.server}
	done := make(chan pluginapi.ModelResponse, 1)
	go func() {
		resp, _ := c.ModelsForAuth(ctx, req)
		done <- resp
	}()
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached the blocking host call")
	}
	cancel()
	select {
	case resp := <-done:
		if !equalIDs(resp.Models, modelIDs(fallbackModels())...) {
			t.Fatalf("models %v want static fallback after cancel", modelIDs(resp.Models))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled call did not return")
	}
	// The handler may record the cancellation slightly after the client
	// returns, so poll briefly instead of asserting a scheduling order.
	deadline := time.Now().Add(5 * time.Second)
	for !f.sawDone.Load() {
		if time.Now().After(deadline) {
			t.Fatal("host client never saw the cancellation")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
