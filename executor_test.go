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
