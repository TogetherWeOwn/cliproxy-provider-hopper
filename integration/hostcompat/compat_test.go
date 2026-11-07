//go:build cgo && (linux || darwin || freebsd)

package hostcompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

type executors map[string]coreauth.ProviderExecutor

func (m executors) Executor(p string) (coreauth.ProviderExecutor, bool) { e, ok := m[p]; return e, ok }
func (m executors) RegisterExecutor(e coreauth.ProviderExecutor)        { m[e.Identifier()] = e }
func (m executors) UnregisterExecutor(p string)                         { delete(m, p) }

type models struct{}

func (models) RegisterClient(string, string, []*registry.ModelInfo) {}
func (models) UnregisterClient(string)                              {}
func (models) GetModelProviders(string) []string                    { return nil }

// The actual v8.0.49 loader performs dlopen, C-ABI registration, RPC dispatch,
// persistence and executor/usage adapters. Only the upstream is an httptest mock.
func TestRealHostLoader(t *testing.T) {
	var polls atomic.Int32
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer example-key" && r.Header.Get("Authorization") != "Bearer second-key" {
			t.Error("incorrect authorization")
		}
		switch r.URL.Path {
		case "/v1/usage":
			polls.Add(1)
			io.WriteString(w, `{"object":"usage","plan":{"key":"pro"},"windows":{"weekly":{"used_usd":30,"limit_usd":40,"resets_at":"2030-01-01T00:00:00Z"}}}`)
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"deepseek-v4.1-flash"},{"id":"deepseek-v4.1-flash-1017"},{"id":"example/model"}]}`)
		case "/v1/chat/completions":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid inference JSON")
				w.WriteHeader(400)
				return
			}
			model, _ := body["model"].(string)
			if model != "deepseek-v4.1-flash" && model != "deepseek-v4.1-flash-1017" && model != "example/model" {
				t.Errorf("model prefix was not stripped: %q", model)
			}
			w.Header().Set("X-Hopper-7d-Used-Usd", "4")
			w.Header().Set("X-Hopper-7d-Limit-Usd", "40")
			if status, ok := body["fixture_status"].(float64); ok && !(body["fixture_first_only"] == true && r.Header.Get("Authorization") == "Bearer second-key") {
				if status == 429 {
					used := "40"
					if body["fixture_concurrent"] == true {
						used = "32"
					}
					w.Header().Set("X-Hopper-7d-Used-Usd", used)
					w.Header().Set("X-Hopper-7d-Reset", "2030-01-01T00:00:00Z")
					w.Header().Set("Retry-After", "7")
				}
				w.WriteHeader(int(status))
				if body["fixture_concurrent"] == true {
					io.WriteString(w, `{"error":"Too many concurrent requests"}`)
				} else {
					io.WriteString(w, `{"error":"fixture upstream error"}`)
				}
				return
			}
			if body["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"id\":\"chat-example\",\"object\":\"chat.completion.chunk\",\"model\":\""+model+"\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"},\"finish_reason\":null}]}\n\n")
				w.(http.Flusher).Flush()
				if body["fixture_gate"] == true {
					select {
					case <-gate:
					case <-r.Context().Done():
						return
					}
				}
				io.WriteString(w, "data: {\"id\":\"chat-example\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chat-example","object":"chat.completion","model":"deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(arch, 0755); err != nil {
		t.Fatal(err)
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	lib := filepath.Join(arch, "hopper"+ext)
	build := exec.Command("go", "build", "-tags=hostcompat", "-buildmode=c-shared", "-o", lib, ".")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("c-shared build: %v\n%s", err, out)
	}
	if out, err := exec.Command("nm", "-D", lib).CombinedOutput(); err != nil || !strings.Contains(string(out), " T cliproxy_plugin_init") {
		t.Fatalf("ABI entry point: %v\n%s", err, out)
	}
	enabled := true
	var raw yaml.Node
	if err = yaml.Unmarshal([]byte("enabled: true\nhostcompat_base: "+server.URL+"\n"), &raw); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{"hopper": {Enabled: &enabled, Raw: *raw.Content[0]}}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = logging.WithResponseHeadersHolder(ctx)
	host.ApplyConfig(ctx, cfg)
	if !host.PluginLoaded("hopper") || !host.PluginRegistered("hopper") || !host.HasAuthProvider("hopper") || !host.HasQuotaProvider("hopper") {
		t.Fatalf("registration %+v", host.RegisteredPlugins())
	}
	info := host.RegisteredPlugins()
	if len(info) != 1 || !info[0].SupportsQuota || info[0].Metadata.Version != "0.1.0" {
		t.Fatalf("metadata %+v", info)
	}
	auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "hopper", FileName: "example.json", RawJSON: []byte(`{"type":"hopper","api_key":"example-key","label":"Example"}`)})
	if err != nil || !handled || auth == nil {
		t.Fatalf("auth %v %v", handled, err)
	}
	if _, _, err = host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "hopper", FileName: "duplicate.json", RawJSON: []byte(`{"type":"hopper","api_key":"example-key"}`)}); err == nil {
		t.Fatal("duplicate key accepted by real host parser")
	}
	file := filepath.Join(cfg.AuthDir, "example.json")
	if err = auth.Storage.SaveTokenToFile(file); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if json.Unmarshal(saved, &fields) != nil || fields["type"] != "hopper" || fields["label"] != "Example" {
		t.Fatal("host auth storage merge failed")
	}
	quotaReq := pluginapi.QuotaFetchRequest{AuthID: auth.ID, Provider: "hopper", StorageJSON: saved}
	quota, handled, err := host.FetchQuota(ctx, quotaReq)
	if err != nil || !handled || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].RemainingFraction != .25 || polls.Load() != 1 {
		t.Fatalf("quota %+v %v %v polls=%d", quota, handled, err, polls.Load())
	}
	m := executors{}
	host.RegisterModels(ctx, models{})
	host.RegisterExecutors(m, models{})
	host.RegisterUsagePlugins()
	static := host.ModelsForProvider("hopper")
	if len(static) != 3 {
		t.Fatalf("static model registration %+v", static)
	}
	for _, model := range static {
		if !strings.HasPrefix(model.ID, "hopper/") {
			t.Fatalf("unprefixed static ID %q", model.ID)
		}
	}
	discovered := host.ModelsForAuth(ctx, auth)
	if !discovered.Handled || len(discovered.Models) != 3 {
		t.Fatalf("model provider %+v", discovered)
	}
	for _, model := range discovered.Models {
		if !strings.HasPrefix(model.ID, "hopper/") {
			t.Fatalf("unprefixed discovery ID %q", model.ID)
		}
	}
	ex, ok := m["hopper"]
	if !ok {
		t.Fatal("executor not registered")
	}
	req := coreexec.Request{Model: "hopper/deepseek-v4.1-flash", Payload: []byte(`{"model":"hopper/deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}]}`), Format: translator.FormatOpenAI}
	opts := coreexec.Options{SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI}
	resp, err := ex.Execute(ctx, auth, req, opts)
	if err != nil || !strings.Contains(string(resp.Payload), `"content":"hello"`) {
		t.Fatalf("execute %s %v", resp.Payload, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		quota, _, err = host.FetchQuota(ctx, quotaReq)
		if err == nil && len(quota.Groups) == 1 && quota.Groups[0].Buckets[0].RemainingFraction == .9 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UsagePlugin did not update quota %+v %v", quota, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if polls.Load() != 1 {
		t.Fatal("header update triggered a quota poll")
	}
	req.Payload = []byte(`{"model":"hopper/deepseek-v4.1-flash","stream":true,"fixture_gate":true,"messages":[{"role":"user","content":"hello"}]}`)
	opts.Stream = true
	stream, err := ex.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var chunks strings.Builder
	select {
	case c := <-stream.Chunks:
		if c.Err != nil || len(c.Payload) == 0 {
			t.Fatalf("first chunk %+v", c)
		}
		chunks.Write(c.Payload)
		close(gate)
	case <-ctx.Done():
		t.Fatal("stream buffered until completion")
	}
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		chunks.Write(c.Payload)
	}
	// The kit re-frames to one data event per chunk and drops the SSE
	// [DONE] marker (the host appends its own terminator): the stream ends
	// by channel close. The core-executor call carries no chat route, so
	// the chunks stay data-framed.
	if want := `data: {"id":"chat-example","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}` +
		`data: {"id":"chat-example","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}` +
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`; chunks.String() != want {
		t.Fatalf("stream %q want %q", chunks.String(), want)
	}
	opts.Stream = false
	for _, tc := range []struct {
		status     int
		concurrent bool
		delay      time.Duration
	}{{401, false, 0}, {402, false, time.Hour}, {403, false, time.Hour}, {404, false, 0}, {429, false, 7 * time.Second}, {429, true, 7 * time.Second}, {503, false, 0}} {
		payload, _ := json.Marshal(map[string]any{"model": "hopper/deepseek-v4.1-flash", "fixture_status": tc.status, "fixture_concurrent": tc.concurrent})
		req.Payload = payload
		_, err = ex.Execute(ctx, auth, req, opts)
		status, ok := err.(interface{ StatusCode() int })
		if !ok || status.StatusCode() != tc.status {
			t.Fatalf("error mapping %d: %T %v", tc.status, err, err)
		}
		if tc.delay > 0 {
			retry, ok := err.(interface{ RetryAfter() *time.Duration })
			if !ok || retry.RetryAfter() == nil || *retry.RetryAfter() != tc.delay {
				t.Fatalf("retry mapping %d: %v", tc.status, err)
			}
		}
	}
	t.Run("native payment failover isolates the failing auth", func(t *testing.T) {
		second, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "hopper", FileName: "second.json", RawJSON: []byte(`{"type":"hopper","api_key":"second-key"}`)})
		if err != nil || !handled || second == nil {
			t.Fatalf("second auth %v", err)
		}
		mgr := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
		mgr.SetRetryConfig(0, 0, 2)
		mgr.RegisterExecutor(ex)
		first := auth.Clone()
		first.ID = "first-auth"
		first.Status = coreauth.StatusActive
		first.Attributes = map[string]string{"type": "api_key", "priority": "10"}
		second = second.Clone()
		second.ID = "second-auth"
		second.Status = coreauth.StatusActive
		for _, a := range []*coreauth.Auth{first, second} {
			if _, err := mgr.Register(ctx, a); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "hopper/deepseek-v4.1-flash"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		}
		call := coreexec.Request{Model: "hopper/deepseek-v4.1-flash", Payload: []byte(`{"model":"hopper/deepseek-v4.1-flash","fixture_status":402,"fixture_first_only":true}`), Format: translator.FormatOpenAI}
		if _, err := mgr.Execute(ctx, []string{"hopper"}, call, opts); err != nil {
			t.Fatalf("native sibling failover %v", err)
		}
		failed, _ := mgr.GetByID(first.ID)
		healthy, _ := mgr.GetByID(second.ID)
		state := failed.ModelStates[call.Model]
		if state == nil || !state.NextRetryAfter.After(time.Now().Add(29*time.Minute)) {
			t.Fatalf("missing failed-auth payment cooldown %+v", state)
		}
		if healthy.Unavailable || healthy.Quota.Exceeded {
			t.Fatalf("healthy sibling impaired %+v", healthy)
		}
	})
	t.Run("pinned host RPC concurrency limitation", func(t *testing.T) {
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		one := auth.Clone()
		one.ID = "concurrency-auth"
		one.Status = coreauth.StatusActive
		if _, err := mgr.Register(ctx, one); err != nil {
			t.Fatal(err)
		}
		model := "hopper/deepseek-v4.1-flash"
		registry.GetGlobalRegistry().RegisterClient(one.ID, one.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(one.ID) })
		_, err := mgr.Execute(ctx, []string{"hopper"}, coreexec.Request{Model: model, Payload: []byte(`{"model":"hopper/deepseek-v4.1-flash","fixture_status":429,"fixture_concurrent":true}`), Format: translator.FormatOpenAI}, opts)
		if err == nil {
			t.Fatal("concurrency failure disappeared")
		}
		got, _ := mgr.GetByID(one.ID)
		state := got.ModelStates[model]
		// This pins an upstream limitation rather than pretending the plugin can
		// carry the native transient marker over ABI 1/schema 10. Remove this
		// characterization when a pinned host exposes that marker in RPC errors.
		if state == nil || !state.Quota.Exceeded {
			t.Fatalf("pinned host behavior changed; revisit documented RPC limitation: %+v", state)
		}
		t.Log("v8.0.49 RPC drops the transient marker and marks concurrency 429 as model quota exceeded")
	})
}

func TestRegistryUsesNativeStoreSchema(t *testing.T) {
	raw, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := pluginstore.ParseRegistry(raw)
	if err != nil || reg.SchemaVersion != 1 || len(reg.Plugins) != 1 || reg.Plugins[0].ID != "hopper" || reg.Plugins[0].Version != "0.1.0" || pluginstore.PluginInstallType(reg.Plugins[0]) != "github-release" {
		t.Fatalf("registry %+v %v", reg, err)
	}
}
