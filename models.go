package main

import (
	"context"
	"net/http"
	"strings"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// staticModels are the chat models Hopper listed for a Pro key on 2026-11-07,
// in upstream (pre-namespace) form. The catalog prefixes them before serving.
func staticModels() []pluginapi.ModelInfo {
	ids := []string{"deepseek-v4.1-flash", "deepseek-v4.1-flash-1017", "deepseek-v4.1-flash-1017-abliterated"}
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	return out
}

// fallbackModels is the static list in served, namespaced form. It is kept
// for existing callers and tests; catalog serving prefixes staticModels.
func fallbackModels() []pluginapi.ModelInfo {
	bare := staticModels()
	out := make([]pluginapi.ModelInfo, 0, len(bare))
	for _, m := range bare {
		m.ID = modelPrefix + m.ID
		out = append(out, m)
	}
	return out
}

// hopperFetch fetches the OpenAI-style id list from GET /v1/models for the
// request's key. The base is resolved on every call so the hostcompat
// loopback override keeps working; the catalog derives its timeout.
func hopperFetch() kit.FetchModelsFunc {
	return func(ctx context.Context, req pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
		key, err := kit.APIKey(req.StorageJSON, req.Metadata)
		if err != nil {
			return nil, err
		}
		fetcher := kit.OpenAIModelsFetcher(selectedBase(upstreamBase), "/v1/models", func(pluginapi.AuthModelRequest) http.Header {
			return http.Header{"Authorization": {"Bearer " + key}}
		})
		return fetcher(ctx, req)
	}
}

// filterChatModels drops embedding ids, normalizes and deduplicates ids,
// and fills the display defaults the former hand-rolled catalog always set.
// It runs after the kit's glob and override passes and before namespace
// prefixing, so dedup applies to the trimmed ids. A single vendor prefix
// already present is stripped before dedup, so a bare id and its prefixed
// twin cannot both be served once the kit namespaces the list.
func filterChatModels(_ context.Context, _ pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		// A singly-prefixed id is normalized to its bare form so the bare
		// and prefixed twins dedupe into one served model. A doubled
		// prefix is left alone so the kit's exactly-once namespace filter
		// rejects it.
		if rest, ok := strings.CutPrefix(id, modelPrefix); ok && !strings.HasPrefix(rest, modelPrefix) {
			id = rest
		}
		// The executor implements chat, not embeddings; the catalog is per
		// key, so entitlement-gated models appear only for keys that can use
		// them.
		if id == "" || strings.Contains(strings.ToLower(id), "embed") {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		m.ID = id
		if m.DisplayName == "" {
			m.DisplayName = id
		}
		if m.OwnedBy == "" {
			m.OwnedBy = provider
		}
		if m.Object == "" {
			m.Object = "model"
		}
		out = append(out, m)
	}
	return out, nil
}

func modelCatalogConfig() kit.ModelCatalogConfig {
	return kit.ModelCatalogConfig{
		Namespace: provider,
		Static:    staticModels(),
		Fetch:     hopperFetch(),
		Validate:  filterChatModels,
	}
}

// newModelCatalog serves models through the kit ModelCatalog: every
// ModelsForAuth fetches fresh for that key, then falls back to the durable
// last-good list and finally to the static list. There is no background
// fetch or retry; recovery waits for the next host event.
func newModelCatalog() *kit.ModelCatalog {
	return kit.NewModelCatalog(modelCatalogConfig())
}

var defaultCatalog = newModelCatalog()

// catalog preserves the pre-migration ModelProvider and ModelRegistrar seams
// for existing callers and tests. Serving delegates to the shared catalog.
type catalog struct{}

func (catalog) RegisterModels(ctx context.Context, req pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return defaultCatalog.RegisterModels(ctx, req)
}
func (catalog) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return defaultCatalog.StaticModels(ctx, req)
}
func (catalog) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return defaultCatalog.ModelsForAuth(ctx, req)
}
