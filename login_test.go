package main

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The kit ships no login flow: API-key providers create an auth JSON file.
// StartLogin and PollLogin fail with api_key_only.
func TestLoginIsAPIKeyOnly(t *testing.T) {
	ctx := context.Background()
	p, err := newAuthProvider()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "hopper"}); err == nil {
		t.Fatal("login started for an API-key-only provider")
	}
	if _, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: "state"}); err == nil {
		t.Fatal("login polled for an API-key-only provider")
	}
}
