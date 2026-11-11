package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This test client substitutes only the transport. It exercises real HTTP,
// streaming body boundaries and cancellation without contacting the provider.
type mockHostClient struct{ server *httptest.Server }

func (c mockHostClient) request(ctx context.Context, r pluginapi.HTTPRequest) (*http.Response, error) {
	if !strings.HasPrefix(r.URL, upstreamBase+"/") {
		return nil, fmt.Errorf("unexpected upstream")
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, c.server.URL+strings.TrimPrefix(r.URL, upstreamBase), bytes.NewReader(r.Body))
	if err != nil {
		return nil, err
	}
	req.Header = r.Headers.Clone()
	return c.server.Client().Do(req)
}
func (c mockHostClient) Do(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	resp, err := c.request(ctx, r)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, err
}
func (c mockHostClient) DoStream(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := c.request(ctx, r)
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	out := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		for {
			buf := make([]byte, 7)
			n, err := resp.Body.Read(buf)
			if n > 0 {
				select {
				case out <- pluginapi.HTTPStreamChunk{Payload: buf[:n]}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case out <- pluginapi.HTTPStreamChunk{Err: err}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Chunks: out}, nil
}

func TestExecutorStreamsBeforeUpstreamCompletes(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := newExecutor(nil).ExecuteStream(ctx, pluginapi.ExecutorRequest{Payload: []byte(`{"model":"hopper/deepseek-v4.1-flash"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case chunk := <-stream.Chunks:
		if chunk.Err != nil || len(chunk.Payload) == 0 {
			t.Fatalf("first chunk %+v", chunk)
		}
	case <-ctx.Done():
		t.Fatal("buffered complete stream")
	}
}

func TestDiscoveryPrefixesFiltersAndDoesNotInventEntitlements(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Error("bad discovery request")
		}
		io.WriteString(w, `{"data":[{"id":"deepseek-v4.1-flash"},{"id":"deepseek-v4.1-flash-1017"},{"id":"example/model"},{"id":"nomic-embed-text-v1.5"},{"id":"deepseek-v4.1-flash"},{"id":""}]}`)
	}))
	defer server.Close()
	out, err := (catalog{}).ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if err != nil || len(out.Models) != 3 {
		t.Fatalf("discovery %+v %v", out, err)
	}
	for i, want := range []string{"hopper/deepseek-v4.1-flash", "hopper/deepseek-v4.1-flash-1017", "hopper/example/model"} {
		if out.Models[i].ID != want {
			t.Fatalf("model %q want %q", out.Models[i].ID, want)
		}
	}
}

// TestStreamScoped429CarriesCredentialScope proves the stream path surfaces
// the same credential-scoped pair as the HTTP path: an upstream 429 with an
// exhausted response-local window on ExecuteStream returns an error that the
// kit runtime maps without losing scope or the reset-derived retry-after.
func TestStreamScoped429CarriesCredentialScope(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Hopper-Plan", "pro")
		w.Header().Set("X-Hopper-5h-Used-Usd", "8.000000")
		w.Header().Set("X-Hopper-5h-Limit-Usd", "8.000000")
		w.Header().Set("X-Hopper-5h-Reset", reset)
		w.Header().Set("X-Hopper-7d-Used-Usd", "1.000000")
		w.Header().Set("X-Hopper-7d-Limit-Usd", "40.000000")
		w.Header().Set("X-Hopper-7d-Reset", reset)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"usage window exhausted"}`)
	}))
	defer server.Close()
	_, err := newExecutor(nil).ExecuteStream(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{"model":"hopper/deepseek-v4.1-flash"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if err == nil {
		t.Fatal("scoped stream 429 returned no error")
	}
	rpcErr, ok := err.(*pluginabi.Error)
	if !ok {
		t.Fatalf("scoped stream error type %T, want *pluginabi.Error", err)
	}
	if rpcErr.HTTPStatus != 429 || rpcErr.Code != "quota_exhausted" {
		t.Fatalf("scoped stream error %+v", rpcErr)
	}
	if !rpcErr.CredentialScoped || !rpcErr.IsCredentialScoped() {
		t.Fatalf("scoped stream CredentialScoped = %v, want true", rpcErr.CredentialScoped)
	}
	if rpcErr.RetryAfterSeconds < 1790 || rpcErr.RetryAfterSeconds > 1805 {
		t.Fatalf("scoped stream RetryAfterSeconds = %d, want roughly 1800", rpcErr.RetryAfterSeconds)
	}
	// The kit runtime sends chunk errors as error_detail through MapError; a
	// mapped *pluginabi.Error passes through with the scoped pair intact.
	remapped := providerkit.MapError(rpcErr)
	if !remapped.CredentialScoped || remapped.RetryAfterSeconds != rpcErr.RetryAfterSeconds {
		t.Fatalf("remapped scoped pair %+v, want CredentialScoped and %d", remapped, rpcErr.RetryAfterSeconds)
	}
}

// TestStreamConcurrency429StaysModelScoped proves the stream path for the
// concurrency shape keeps the 5s model-scoped behavior unchanged.
func TestStreamConcurrency429StaysModelScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Hopper-5h-Used-Usd", "1.000000")
		w.Header().Set("X-Hopper-5h-Limit-Usd", "8.000000")
		w.Header().Set("X-Hopper-5h-Reset", "2030-01-01T05:00:00Z")
		w.Header().Set("X-Hopper-7d-Used-Usd", "1.000000")
		w.Header().Set("X-Hopper-7d-Limit-Usd", "40.000000")
		w.Header().Set("X-Hopper-7d-Reset", "2030-01-06T00:00:00Z")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"Too many concurrent requests"}`)
	}))
	defer server.Close()
	_, err := newExecutor(nil).ExecuteStream(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{"model":"hopper/deepseek-v4.1-flash"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if err == nil {
		t.Fatal("concurrency stream 429 returned no error")
	}
	rpcErr, ok := err.(*pluginabi.Error)
	if !ok {
		t.Fatalf("concurrency stream error type %T, want *pluginabi.Error", err)
	}
	if rpcErr.HTTPStatus != 429 || rpcErr.Code != "concurrency_limit" || rpcErr.RetryAfterSeconds != 5 {
		t.Fatalf("concurrency stream error %+v", rpcErr)
	}
	if rpcErr.CredentialScoped || rpcErr.IsCredentialScoped() {
		t.Fatalf("concurrency stream CredentialScoped = %v, want false", rpcErr.CredentialScoped)
	}
	remapped := providerkit.MapError(rpcErr)
	if remapped.CredentialScoped || remapped.RetryAfterSeconds != 5 {
		t.Fatalf("remapped concurrency %+v, want unscoped with 5s", remapped)
	}
}

// TestMapErrorKeepsScopedPairForErrorDetail pins the exact reset arithmetic
// through the sanitizing map: the scoped classify result survives MapError
// with the same reset-derived seconds under a pinned now.
func TestMapErrorKeepsScopedPairForErrorDetail(t *testing.T) {
	now := time.Date(2030, 1, 1, 4, 0, 0, 0, time.UTC)
	wrapped := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: hopperHeaders("8", "1")}, now, nil)
	if wrapped.RetryAfterSeconds != 3600 || !wrapped.CredentialScoped {
		t.Fatalf("wrapped %+v", wrapped)
	}
	remapped := providerkit.MapError(wrapped)
	if !remapped.CredentialScoped || remapped.RetryAfterSeconds != 3600 || remapped.HTTPStatus != 429 || remapped.Code != "quota_exhausted" {
		t.Fatalf("remapped %+v", remapped)
	}
}
