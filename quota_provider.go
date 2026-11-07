package main

import (
	"context"
	"sync"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaProvider shares the same cache with the UsagePlugin, keyed by auth ID.
type quotaProvider struct {
	cache    *providerkit.CachedQuota
	mu       sync.Mutex
	readings map[string]timedReading
	now      func() time.Time
}

type timedReading struct {
	usage      quotaUsage
	observedAt time.Time
	expires    time.Time
}

func (q *quotaProvider) clock() time.Time {
	if q != nil && q.now != nil {
		return q.now()
	}
	return time.Now()
}

// These observations are response-classification hints only. They never select
// an account, avoid an account preemptively, or alter the host's scheduler.
func (q *quotaProvider) remember(id string, usage quotaUsage, observedAt time.Time) {
	if id == "" || len(usage.Readings) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clock()
	if q.readings == nil {
		q.readings = make(map[string]timedReading)
	}
	for key, r := range q.readings {
		if !now.Before(r.expires) {
			delete(q.readings, key)
		}
	}
	current, ok := q.readings[id]
	if ok && current.observedAt.After(observedAt) {
		return
	}
	kept := quotaUsage{Plan: usage.Plan, BalanceUSD: usage.BalanceUSD, Readings: append([]quotaReading(nil), usage.Readings...)}
	if ok && kept.Plan == "" {
		kept.Plan = current.usage.Plan
	}
	// Headers never carry the balance, so a header update keeps the polled one.
	if ok && kept.BalanceUSD == nil {
		kept.BalanceUSD = current.usage.BalanceUSD
	}
	q.readings[id] = timedReading{usage: kept, observedAt: observedAt, expires: observedAt.Add(providerkit.QuotaCacheTTL)}
}

func (q *quotaProvider) latest(id string) []quotaReading {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok := q.readings[id]
	if !ok || !q.clock().Before(r.expires) {
		delete(q.readings, id)
		return nil
	}
	return append([]quotaReading(nil), r.usage.Readings...)
}

func quotaIdentity(id, index string) string {
	if id != "" {
		return id
	}
	return index
}

func newQuotaProvider(endpoint string, client httpDoer) *quotaProvider {
	q := &quotaProvider{}
	q.cache = providerkit.NewCachedQuota(provider, "Hopper", func(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		key, err := providerkit.APIKey(req.StorageJSON, req.Metadata)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		url := endpoint
		if url == "" {
			url = selectedBase(upstreamBase) + "/v1/usage"
		}
		// A header received while this poll is in flight is newer evidence.
		observedAt := time.Now()
		usage, err := fetchUsage(ctx, client, url, key)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		q.remember(quotaIdentity(req.AuthID, req.AuthIndex), usage, observedAt)
		return normalizeUsage(usage), nil
	})
	return q
}

// normalizeUsage renders one Hopper group with a bucket per spend window.
// Both windows reset fully, so each bucket keeps its reset time.
func normalizeUsage(usage quotaUsage) pluginapi.QuotaFetchResponse {
	out := pluginapi.QuotaFetchResponse{}
	var buckets []pluginapi.QuotaBucket
	for _, r := range usage.Readings {
		window, label := providerkit.CardWindowWeekly, "Weekly limit"
		if r.Window == windowFiveHour {
			window, label = providerkit.CardWindowFiveHour, "5-hour limit"
		}
		bucket := pluginapi.QuotaBucket{Window: window, RemainingFraction: r.remainingFraction(), Description: label}
		if !r.ResetAt.IsZero() {
			bucket.ResetTime = r.ResetAt.Format(time.RFC3339Nano)
		}
		buckets = append(buckets, bucket)
	}
	if len(buckets) > 0 {
		out.Groups = []pluginapi.QuotaGroup{{DisplayName: "Hopper", Buckets: buckets}}
	}
	if usage.Plan != "" {
		out.Subscription = &pluginapi.QuotaSubscription{Plan: usage.Plan}
	}
	if usage.BalanceUSD != nil {
		out.Summary = []pluginapi.QuotaMetric{{Key: "account_balance_usd", Label: "Balance", Value: *usage.BalanceUSD, Format: "currency", Currency: "USD"}}
	}
	return out
}

// HandleUsage consumes only provider-matched response headers and never persists keys.
func (q *quotaProvider) HandleUsage(_ context.Context, record pluginapi.UsageRecord) {
	if record.Provider != provider {
		return
	}
	usage, ok := usageFromHeaders(record.ResponseHeaders)
	if !ok {
		return
	}
	id := quotaIdentity(record.AuthID, record.AuthIndex)
	q.remember(id, usage, time.Now())
	q.mu.Lock()
	if remembered, ok := q.readings[id]; ok {
		if usage.Plan == "" {
			usage.Plan = remembered.usage.Plan
		}
		usage.BalanceUSD = remembered.usage.BalanceUSD
	}
	q.mu.Unlock()
	_ = q.cache.Store(pluginapi.QuotaFetchRequest{Provider: record.Provider, AuthID: record.AuthID, AuthIndex: record.AuthIndex}, normalizeUsage(usage))
}
