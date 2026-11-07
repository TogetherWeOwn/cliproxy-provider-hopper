package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func classifyResponse(r pluginapi.HTTPResponse) error { return classifyFailure(r, time.Now()) }

type executor struct {
	kit.Passthrough
	quota *quotaProvider
}

func newExecutor(q *quotaProvider) executor {
	return executor{Passthrough: kit.Passthrough{Provider: provider, BuildRequest: buildRequest, PrepareHTTP: prepareHTTP, ClassifyError: classifyResponse}, quota: q}
}

// forAuth lets a 429 without usage headers borrow the last observed windows
// for its RetryAfter hint.
func (e executor) forAuth(id string) kit.Passthrough {
	p := e.Passthrough
	p.ClassifyError = func(resp pluginapi.HTTPResponse) error {
		return classifyWithQuota(resp, time.Now(), e.quota.latest(id))
	}
	return p
}

func (e executor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	r.Stream = false
	return e.forAuth(r.AuthID).Execute(ctx, r)
}

func (e executor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	r.Stream = true
	return e.forAuth(r.AuthID).ExecuteStream(ctx, r)
}

func (e executor) HttpRequest(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return e.forAuth(r.AuthID).HttpRequest(ctx, r)
}

func buildRequest(_ context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
	key, err := kit.APIKey(r.StorageJSON, r.AuthMetadata)
	if err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(r.Payload, &body) != nil || body == nil {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_request", Message: "payload must be a JSON object", HTTPStatus: 400}
	}
	var model string
	if json.Unmarshal(body["model"], &model) != nil || !strings.HasPrefix(model, modelPrefix) || len(model) == len(modelPrefix) {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_model", Message: "expected a hopper/ model", HTTPStatus: 400}
	}
	body["model"], _ = json.Marshal(strings.TrimPrefix(model, modelPrefix))
	if r.Stream {
		body["stream"] = json.RawMessage("true")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: selectedBase(upstreamBase) + "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + key}, "Content-Type": {"application/json"}}, Body: raw}, nil
}

func prepareHTTP(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error) {
	// Raw HTTP execution cannot inject destinations, credentials or extra headers.
	return buildRequest(ctx, pluginapi.ExecutorRequest{Payload: r.Body, StorageJSON: r.StorageJSON, AuthMetadata: r.Metadata})
}
