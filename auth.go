package main

import (
	"context"
	"crypto/sha256"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Ownership is by filename, so a watcher's repeated parse of one file is safe.
// Only digests are retained; keys never appear in duplicate-file errors.
// kit.KeyOwnership keeps that split provisional-aware: host parses of real
// auth files commit ownership, while a login-validation parse only reserves
// the digest until the login session expires, so an abandoned login releases
// the key instead of holding it until plugin restart.
type authProvider struct {
	kit.APIKeyAuth
	guard kit.KeyOwnership
}

func newAuthProvider() (*authProvider, error) {
	return newAuthProviderWithLoginTTL(0)
}

// newAuthProviderWithLoginTTL exists for tests that need a login session to
// expire promptly; a zero TTL keeps the kit default.
func newAuthProviderWithLoginTTL(loginTTL time.Duration) (*authProvider, error) {
	p := &authProvider{APIKeyAuth: kit.APIKeyAuth{Provider: provider, Validate: func(m map[string]any) error {
		if _, ok := m["id"]; ok {
			return &pluginabi.Error{Code: "invalid_auth", Message: "auth identity is derived from the API key", HTTPStatus: 400}
		}
		return nil
	}}}
	// Parse is the full ParseAuth pipeline below, including the duplicate-key
	// guard, so a submitted key faces the same checks as an uploaded auth file.
	login, err := kit.NewAPIKeyLogin(kit.APIKeyLoginConfig{Provider: provider, Parse: p.ParseAuth, SessionTTL: loginTTL})
	if err != nil {
		return nil, err
	}
	p.APIKeyAuth.Login = login
	return p, nil
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
