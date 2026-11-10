package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAuthDuplicateKeyAndReload(t *testing.T) {
	p, err := newAuthProvider()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	parse := func(file, raw string) (pluginapi.AuthParseResponse, error) {
		return p.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "hopper", FileName: file, RawJSON: []byte(raw)})
	}
	a, err := parse("first.json", `{"type":"hopper","api_key":"example-key","weight":2}`)
	if err != nil || !a.Handled || a.Auth.ID == "" {
		t.Fatalf("parse %+v %v", a, err)
	}
	if _, err = parse("second.json", `{"type":"hopper","api_key":"example-key"}`); err == nil {
		t.Fatal("duplicate key in a second file accepted")
	}
	b, err := parse("first.json", `{"type":"hopper","api_key":" example-key "}`)
	if err != nil || b.Auth.ID != a.Auth.ID {
		t.Fatalf("reload %+v %v", b, err)
	}
	if _, err = parse("first.json", `{"type":"hopper","api_key":"replacement-key"}`); err != nil {
		t.Fatal(err)
	}
	if _, err = parse("second.json", `{"type":"hopper","api_key":"example-key"}`); err != nil {
		t.Fatal("old key ownership not released", err)
	}
	for _, raw := range []string{`{"type":"hopper","api_key":"a","id":"custom"}`, `{"type":"hopper","api_key":""}`} {
		if _, err = parse("invalid.json", raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	// The kit ships no login flow: StartLogin fails with api_key_only.
	if _, err = p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "hopper"}); err == nil {
		t.Fatal("login started for an API-key-only provider")
	}
	refreshed, err := p.RefreshAuth(ctx, pluginapi.AuthRefreshRequest{AuthID: a.Auth.ID, StorageJSON: a.Auth.StorageJSON, Metadata: a.Auth.Metadata})
	if err != nil || string(refreshed.Auth.StorageJSON) != string(a.Auth.StorageJSON) {
		t.Fatalf("refresh %+v %v", refreshed, err)
	}
}

func TestBuildRequestStripsOnlyProviderPrefixAndPreservesPayload(t *testing.T) {
	for _, tc := range []struct {
		model      string
		stream     bool
		wantStream bool
	}{
		{"hopper/deepseek-v4.1-flash", false, false},
		{"hopper/deepseek-v4.1-flash-1017", false, false},
		{"hopper/example/model", true, true},
	} {
		payload, _ := json.Marshal(map[string]any{"model": tc.model, "messages": []any{}, "stream": tc.stream, "temperature": 0.25, "custom": map[string]any{"preserve": true}})
		got, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: payload, StorageJSON: []byte(`{"api_key":"example-key"}`)})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if json.Unmarshal(got.Body, &body) != nil {
			t.Fatal("invalid JSON")
		}
		if got.URL != "https://api.withhopper.com/v1/chat/completions" || got.Method != "POST" || got.Headers.Get("Authorization") != "Bearer example-key" {
			t.Fatalf("bad upstream request %+v", got)
		}
		if body["model"] != tc.model[len("hopper/"):] || body["stream"] != tc.wantStream || body["temperature"] != 0.25 || body["custom"].(map[string]any)["preserve"] != true {
			t.Fatalf("changed payload %s", got.Body)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"model":"deepseek-v4.1-flash"}`, `{"model":"hopper/"}`} {
		if _, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(raw), StorageJSON: []byte(`{"api_key":"example-key"}`)}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestStaticCatalogOnlyPrefixedChatModels(t *testing.T) {
	c := catalog{}
	static, err := c.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	if err != nil || len(static.Models) != 3 {
		t.Fatalf("catalog %+v %v", static, err)
	}
	reg, err := c.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if err != nil || len(reg.Models) != len(static.Models) {
		t.Fatalf("registration %+v %v", reg, err)
	}
	for _, m := range static.Models {
		if len(m.ID) <= len("hopper/") || m.ID[:len("hopper/")] != "hopper/" {
			t.Fatalf("unprefixed model %q", m.ID)
		}
	}
}

func TestPrepareHTTPDoesNotForwardUntrustedURLOrHeaders(t *testing.T) {
	req, err := prepareHTTP(context.Background(), pluginapi.ExecutorHTTPRequest{Method: "DELETE", URL: "https://example.invalid/private", Headers: http.Header{"Authorization": {"Bearer attacker"}}, Body: []byte(`{"model":"hopper/deepseek-v4.1-flash"}`), StorageJSON: []byte(`{"api_key":"example-key"}`)})
	if err != nil || req.URL != "https://api.withhopper.com/v1/chat/completions" || req.Method != "POST" || req.Headers.Get("Authorization") != "Bearer example-key" {
		t.Fatalf("unsafe HTTP request %+v %v", req, err)
	}
	var body map[string]any
	_ = json.Unmarshal(req.Body, &body)
	if body["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("prefix not stripped: %s", req.Body)
	}
}
