package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRawHTTPIgnoresUntrustedDestinationAndStaysNonStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer example-key" || strings.Contains(string(body), `"stream":true`) {
			t.Errorf("unsafe raw execution %s %s", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"hello"}}]}`)
	}))
	defer server.Close()
	result, err := newExecutor(nil).HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Method: "DELETE", URL: "https://example.invalid/ignored", Headers: http.Header{"Authorization": {"Bearer untrusted"}}, Body: []byte(`{"model":"hopper/deepseek-v4.1-flash-1017","stream":false}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if err != nil || result.StatusCode != 200 || !strings.Contains(string(result.Body), `"content":"hello"`) {
		t.Fatalf("raw request %s %+v %v", result.Body, result.Headers, err)
	}
}

func TestExecutorTokenCountIsLocalAndEstimated(t *testing.T) {
	out, err := newExecutor(nil).CountTokens(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{"model":"hopper/deepseek-v4.1-flash"}`)})
	if err != nil || out.Metadata["estimated"] != true || !strings.Contains(string(out.Payload), `"total_tokens":`) {
		t.Fatalf("count %+v %v", out, err)
	}
}
