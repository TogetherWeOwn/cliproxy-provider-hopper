package main

import (
	"context"
	"crypto/sha256"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Ownership is by filename, so a watcher's repeated parse of one file is safe.
// Only digests are retained; keys never appear in duplicate-file errors.
// kit.KeyOwnership keeps that split provisional-aware.
type authProvider struct {
	kit.APIKeyAuth
	guard kit.KeyOwnership
}

func newAuthProvider() (*authProvider, error) {
	return &authProvider{APIKeyAuth: kit.APIKeyAuth{Provider: provider, Validate: func(m map[string]any) error {
		if _, ok := m["id"]; ok {
			return &pluginabi.Error{Code: "invalid_auth", Message: "auth identity is derived from the API key", HTTPStatus: 400}
		}
		return nil
	}}}, nil
}

func (p *authProvider) ParseAuth(ctx context.Context, r pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	out, err := p.APIKeyAuth.ParseAuth(ctx, r)
	if err != nil || !out.Handled {
		return out, err
	}
	key, err := kit.APIKey(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	digest := sha256.Sum256([]byte(key))
	if err := p.guard.Claim(ctx, r.FileName, digest); err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	return out, nil
}
