package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
)

const (
	maxQuotaResponse    = 1 << 20
	defaultQuotaTimeout = 15 * time.Second
	windowFiveHour      = "five_hour"
	windowWeekly        = "weekly"
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// quotaReading is one Hopper spend window. Hopper meters each window in USD
// and resets it fully at ResetAt; the parser never fabricates a value.
type quotaReading struct {
	Window   string
	UsedUSD  float64
	LimitUSD float64
	ResetAt  time.Time
}

func (q quotaReading) remainingFraction() float64 {
	return clamp((q.LimitUSD-q.UsedUSD)/q.LimitUSD, 0, 1)
}

func (q quotaReading) exhausted() bool { return q.UsedUSD >= q.LimitUSD }

// quotaUsage is one observation: the plan key, the prepaid credit balance
// (only /v1/usage reports it) and every window reported.
type quotaUsage struct {
	Plan       string
	BalanceUSD *float64
	Readings   []quotaReading
}

type usageWindowWire struct {
	UsedUSD  *float64 `json:"used_usd"`
	LimitUSD *float64 `json:"limit_usd"`
	ResetsAt string   `json:"resets_at"`
}

// usageWire is the GET /v1/usage shape observed on 2026-11-07.
type usageWire struct {
	Plan *struct {
		Key string `json:"key"`
	} `json:"plan"`
	Windows struct {
		FiveHour *usageWindowWire `json:"five_hour"`
		Weekly   *usageWindowWire `json:"weekly"`
	} `json:"windows"`
	CreditsRemainingUSD *float64 `json:"credits_remaining_usd"`
}

var errNoQuotaShape = errors.New("usage payload has no recognized window")

func newReading(window string, used, limit *float64, reset string) (quotaReading, bool) {
	if used == nil || limit == nil || *limit <= 0 || *used < 0 {
		return quotaReading{}, false
	}
	return quotaReading{Window: window, UsedUSD: *used, LimitUSD: *limit, ResetAt: parseTime(reset)}, true
}

// parseUsage returns the five-hour then weekly window from a /v1/usage body.
func parseUsage(raw []byte) (quotaUsage, error) {
	var wire usageWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return quotaUsage{}, fmt.Errorf("decode usage payload")
	}
	out := quotaUsage{BalanceUSD: wire.CreditsRemainingUSD}
	if wire.Plan != nil {
		out.Plan = strings.TrimSpace(wire.Plan.Key)
	}
	for _, w := range []struct {
		name string
		wire *usageWindowWire
	}{{windowFiveHour, wire.Windows.FiveHour}, {windowWeekly, wire.Windows.Weekly}} {
		if w.wire == nil {
			continue
		}
		if r, ok := newReading(w.name, w.wire.UsedUSD, w.wire.LimitUSD, w.wire.ResetsAt); ok {
			out.Readings = append(out.Readings, r)
		}
	}
	if len(out.Readings) == 0 {
		return quotaUsage{}, errNoQuotaShape
	}
	return out, nil
}

func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func headerFloat(headers http.Header, name string) *float64 {
	v, err := strconv.ParseFloat(headerValue(headers, name), 64)
	if err != nil {
		return nil
	}
	return &v
}

// usageFromHeaders reads the x-hopper-{5h,7d}-* headers Hopper attaches to
// inference responses. It returns ok=false when no window is reported.
func usageFromHeaders(headers http.Header) (quotaUsage, bool) {
	out := quotaUsage{Plan: headerValue(headers, "X-Hopper-Plan")}
	for _, w := range []struct{ prefix, name string }{{"X-Hopper-5h-", windowFiveHour}, {"X-Hopper-7d-", windowWeekly}} {
		if r, ok := newReading(w.name, headerFloat(headers, w.prefix+"Used-Usd"), headerFloat(headers, w.prefix+"Limit-Usd"), headerValue(headers, w.prefix+"Reset")); ok {
			out.Readings = append(out.Readings, r)
		}
	}
	return out, len(out.Readings) > 0
}

// retryAt is when every exhausted window has reset: the latest reset among
// exhausted windows, or zero when none is exhausted or a reset is unknown.
func retryAt(readings []quotaReading) time.Time {
	var at time.Time
	for _, r := range readings {
		if !r.exhausted() {
			continue
		}
		if r.ResetAt.IsZero() {
			return time.Time{}
		}
		if r.ResetAt.After(at) {
			at = r.ResetAt
		}
	}
	return at
}

// newQuotaHTTPClient follows the proxy that the kit's CachedQuota attaches to
// each poll: the auth's proxy_url, else the host's global proxy-url. The kit
// transport keeps pooled connections from crossing proxies.
func newQuotaHTTPClient() *http.Client {
	client := &http.Client{Transport: providerkit.NewQuotaTransport(), Timeout: defaultQuotaTimeout}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

// fetchUsage performs one bounded GET of the usage endpoint. Errors are
// generic: neither the API key nor the response body is ever included.
func fetchUsage(ctx context.Context, client httpDoer, endpoint, apiKey string) (quotaUsage, error) {
	if client == nil || endpoint == "" || apiKey == "" {
		return quotaUsage{}, fmt.Errorf("quota poll not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultQuotaTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return quotaUsage{}, fmt.Errorf("build quota request")
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return quotaUsage{}, fmt.Errorf("quota request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaResponse+1))
	if err != nil {
		return quotaUsage{}, fmt.Errorf("read quota response")
	}
	if len(body) > maxQuotaResponse {
		return quotaUsage{}, fmt.Errorf("quota response too large")
	}
	if resp.StatusCode != http.StatusOK {
		return quotaUsage{}, fmt.Errorf("quota endpoint returned HTTP %d", resp.StatusCode)
	}
	return parseUsage(body)
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func clamp(value, lo, hi float64) float64 {
	if value < lo {
		return lo
	}
	if value > hi {
		return hi
	}
	return value
}
