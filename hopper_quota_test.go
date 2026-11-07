package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usageBody is the GET /v1/usage shape observed on 2026-11-07, values changed.
const usageBody = `{"object":"usage","plan":{"key":"pro","name":"Pro","status":"active"},
"windows":{"five_hour":{"used_usd":2,"limit_usd":8,"started_at":"2030-01-01T00:00:00Z","resets_at":"2030-01-01T05:00:00Z"},
"weekly":{"used_usd":30,"limit_usd":40,"started_at":"2029-12-30T00:00:00Z","resets_at":"2030-01-06T00:00:00Z"}},
"covered_models":[{"id":"deepseek-v4.1-flash","weight":1}],"credits_remaining_usd":5,"grace_limit_usd":0}`

func hopperHeaders(used5h, used7d string) http.Header {
	h := http.Header{}
	h.Set("x-hopper-plan", "pro")
	h.Set("x-hopper-5h-used-usd", used5h)
	h.Set("x-hopper-5h-limit-usd", "8.000000")
	h.Set("x-hopper-5h-reset", "2030-01-01T05:00:00Z")
	h.Set("x-hopper-7d-used-usd", used7d)
	h.Set("x-hopper-7d-limit-usd", "40.000000")
	h.Set("x-hopper-7d-reset", "2030-01-06T00:00:00Z")
	return h
}

func TestParseUsageWindowsAndPlan(t *testing.T) {
	u, err := parseUsage([]byte(usageBody))
	if err != nil || u.Plan != "pro" || len(u.Readings) != 2 {
		t.Fatalf("usage %+v %v", u, err)
	}
	five, week := u.Readings[0], u.Readings[1]
	if five.Window != windowFiveHour || five.remainingFraction() != 0.75 || !five.ResetAt.Equal(time.Date(2030, 1, 1, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("five-hour %+v", five)
	}
	if week.Window != windowWeekly || week.remainingFraction() != 0.25 {
		t.Fatalf("weekly %+v", week)
	}
	for _, raw := range []string{`{}`, `{"windows":{"weekly":{"used_usd":1,"limit_usd":0}}}`, `{"windows":{"weekly":{"limit_usd":5}}}`, `not json`} {
		if _, err := parseUsage([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestUsageFromHeadersCaseInsensitive(t *testing.T) {
	h := http.Header{}
	for k, v := range hopperHeaders("8", "1") {
		h[strings.ToLower(k)] = v // non-canonical keys as some transports deliver them
	}
	u, ok := usageFromHeaders(h)
	if !ok || u.Plan != "pro" || len(u.Readings) != 2 || !u.Readings[0].exhausted() || u.Readings[1].exhausted() {
		t.Fatalf("headers %+v %v", u, ok)
	}
	if _, ok := usageFromHeaders(http.Header{"Content-Type": {"application/json"}}); ok {
		t.Fatal("usage invented from unrelated headers")
	}
}

func TestRetryAtWaitsForEveryExhaustedWindow(t *testing.T) {
	u, _ := usageFromHeaders(hopperHeaders("8", "40"))
	if got := retryAt(u.Readings); !got.Equal(time.Date(2030, 1, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("retryAt %v", got)
	}
	u, _ = usageFromHeaders(hopperHeaders("1", "1"))
	if !retryAt(u.Readings).IsZero() {
		t.Fatal("retry time without an exhausted window")
	}
}

func TestClassify429(t *testing.T) {
	now := time.Date(2030, 1, 1, 4, 0, 0, 0, time.UTC)
	exhausted := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: hopperHeaders("8", "1")}, now, nil)
	if exhausted.Code != "quota_exhausted" || exhausted.RetryAfterSeconds != 3600 || !exhausted.Retryable {
		t.Fatalf("exhausted %+v", exhausted)
	}
	concurrent := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: hopperHeaders("1", "1"), Body: []byte(`{"error":"too many concurrent requests"}`)}, now, nil)
	if concurrent.Code != "concurrency_limit" || concurrent.RetryAfterSeconds != 5 {
		t.Fatalf("concurrent %+v", concurrent)
	}
	// Cached headroom never downgrades a header-less 429 to concurrency.
	cached, _ := usageFromHeaders(hopperHeaders("1", "1"))
	bare := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Body: []byte(`concurrency`)}, now, cached.Readings)
	if bare.Code != "quota_exhausted" || bare.RetryAfterSeconds != 60 {
		t.Fatalf("bare %+v", bare)
	}
	// A header-less 429 borrows cached exhaustion for its RetryAfter hint.
	full, _ := usageFromHeaders(hopperHeaders("8", "1"))
	if e := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429}, now, full.Readings); e.RetryAfterSeconds != 3600 {
		t.Fatalf("cached %+v", e)
	}
	h := hopperHeaders("8", "1")
	h.Set("Retry-After", "7")
	if e := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: h}, now, nil); e.RetryAfterSeconds != 7 {
		t.Fatalf("retry-after %+v", e)
	}
}

func TestClassifyStatusesNeverEchoBody(t *testing.T) {
	for status, code := range map[int]string{401: "invalid_credentials", 402: "payment_required", 403: "entitlement_required", 404: "upstream_not_found", 500: "upstream_error"} {
		e := classifyFailure(pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{}, Body: []byte(`secret-body sk_hopper_x`)}, time.Now())
		if e.Code != code || e.HTTPStatus != status || strings.Contains(e.Message, "secret") || !strings.HasPrefix(e.Message, "Hopper") {
			t.Fatalf("%d: %+v", status, e)
		}
		if e.Retryable != (status >= 500) {
			t.Fatalf("%d retryable %v", status, e.Retryable)
		}
	}
}

func TestQuotaPollRendersHopperCard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Error("bad usage request")
		}
		w.Write([]byte(usageBody))
	}))
	defer server.Close()
	q := newQuotaProvider(server.URL+"/v1/usage", server.Client())
	out, err := q.cache.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "a", StorageJSON: []byte(`{"api_key":"example-key"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Subscription == nil || out.Subscription.Plan != "pro" || len(out.Groups) != 1 || out.Groups[0].DisplayName != "Hopper" || len(out.Groups[0].Buckets) != 2 {
		t.Fatalf("card %+v", out)
	}
	b := out.Groups[0].Buckets
	if b[0].Window != kit.CardWindowFiveHour || b[0].RemainingFraction != 0.75 || b[0].ResetTime != "2030-01-01T05:00:00Z" || b[0].Description != "$2.00 of $8.00 used" {
		t.Fatalf("five-hour bucket %+v", b[0])
	}
	if b[1].Window != kit.CardWindowWeekly || b[1].RemainingFraction != 0.25 {
		t.Fatalf("weekly bucket %+v", b[1])
	}
	if len(q.latest("a")) != 2 {
		t.Fatal("poll not remembered for 429 hints")
	}
}

func TestHandleUsageIgnoresOtherProviders(t *testing.T) {
	q := newQuotaProvider("", nil)
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "synthetic", AuthID: "a", ResponseHeaders: hopperHeaders("1", "1")})
	if q.latest("a") != nil {
		t.Fatal("foreign usage remembered")
	}
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: provider, AuthID: "a", ResponseHeaders: hopperHeaders("8", "1")})
	if r := q.latest("a"); len(r) != 2 || !r[0].exhausted() {
		t.Fatalf("hopper usage %+v", r)
	}
}
