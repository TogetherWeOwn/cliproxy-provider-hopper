package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type catalog struct{}

// fallbackModels are the chat models Hopper listed for a Pro key on 2026-11-07.
func fallbackModels() []pluginapi.ModelInfo {
	ids := []string{"deepseek-v4.1-flash", "deepseek-v4.1-flash-1017", "deepseek-v4.1-flash-1017-abliterated"}
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: modelPrefix + id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	return out
}
func (catalog) RegisterModels(context.Context, pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return pluginapi.ModelRegistrationResponse{Provider: provider, Models: fallbackModels()}, nil
}
func (catalog) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: provider, Models: fallbackModels()}, nil
}
func (catalog) ModelsForAuth(ctx context.Context, r pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	key, err := kit.APIKey(r.StorageJSON, r.Metadata)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	if r.HTTPClient == nil {
		return pluginapi.ModelResponse{}, &pluginabi.Error{Code: "host_unavailable", Message: "model discovery requires the host HTTP client", HTTPStatus: 503}
	}
	resp, err := r.HTTPClient.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: selectedBase(upstreamBase) + "/v1/models", Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}})
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.ModelResponse{}, classifyResponse(resp)
	}
	var wire struct {
		Data []struct {
			ID string `json:"id"`
		}
	}
	if len(resp.Body) > maxQuotaResponse || json.Unmarshal(resp.Body, &wire) != nil || wire.Data == nil {
		return pluginapi.ModelResponse{}, &pluginabi.Error{Code: "invalid_models", Message: "Hopper returned an invalid model catalog", HTTPStatus: 502}
	}
	out := pluginapi.ModelResponse{Provider: provider}
	seen := make(map[string]bool)
	for _, model := range wire.Data {
		id := strings.TrimSpace(model.ID)
		// The executor implements chat, not embeddings; the catalog is per key,
		// so entitlement-gated models appear only for keys that can use them.
		if id == "" || strings.Contains(strings.ToLower(id), "embed") || strings.HasPrefix(id, modelPrefix) || seen[id] {
			continue
		}
		seen[id] = true
		out.Models = append(out.Models, pluginapi.ModelInfo{ID: modelPrefix + id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	return out, nil
}
