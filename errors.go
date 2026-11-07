package main

import (
	"math"
	"net/http"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// No raw body or credential is copied into an RPC error.
func classifyFailure(resp pluginapi.HTTPResponse, now time.Time) *pluginabi.Error {
	return classifyWithQuota(resp, now, nil)
}

// classifyWithQuota maps an upstream failure. cached windows only fill a
// 429's RetryAfter hint when the response itself carries no usage headers.
func classifyWithQuota(resp pluginapi.HTTPResponse, now time.Time, cached []quotaReading) *pluginabi.Error {
	e := &pluginabi.Error{Code: "upstream_error", Message: "Hopper request failed", HTTPStatus: resp.StatusCode, RetryAfterSeconds: kit.RetryAfterSeconds(resp.Headers.Get("Retry-After"), now)}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Code, e.Message, e.RetryAfterSeconds = "invalid_credentials", "Hopper rejected the credential", 0
	case http.StatusPaymentRequired:
		e.Code, e.Message, e.RetryAfterSeconds = "payment_required", "Hopper reported a payment issue", 3600
	case http.StatusForbidden:
		e.Code, e.Message, e.RetryAfterSeconds = "entitlement_required", "Hopper requires a different entitlement", 3600
	case http.StatusNotFound:
		e.Code, e.Message, e.RetryAfterSeconds = "upstream_not_found", "Hopper endpoint or model was not found", 0
	case http.StatusTooManyRequests:
		e.Retryable = true
		usage, hasHeader := usageFromHeaders(resp.Headers)
		readings := usage.Readings
		if !hasHeader {
			readings = cached
		}
		body := strings.ToLower(string(resp.Body))
		// Only response-local headers showing no exhausted window vouch for
		// concurrency; cached headroom must not downgrade a real exhaustion.
		if hasHeader && !anyExhausted(readings) && strings.Contains(body, "concurren") {
			e.Code, e.Message, e.RetryAfterSeconds = "concurrency_limit", "Hopper concurrency limit reached", 5
		} else {
			e.Code, e.Message, e.RetryAfterSeconds = "quota_exhausted", "Hopper usage window or rate limit reached", 60
			if at := retryAt(readings); at.After(now) {
				seconds := math.Ceil(at.Sub(now).Seconds())
				if seconds > math.MaxInt32 {
					seconds = math.MaxInt32
				}
				e.RetryAfterSeconds = int(seconds)
			}
		}
		if n := kit.RetryAfterSeconds(resp.Headers.Get("Retry-After"), now); n > 0 {
			e.RetryAfterSeconds = n
		}
	default:
		e.Retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout
	}
	return e
}

func anyExhausted(readings []quotaReading) bool {
	for _, r := range readings {
		if r.exhausted() {
			return true
		}
	}
	return false
}
