package main

// Contract test for the org PR-size caller (see the org standard,
// "PR size gate"). The caller keeps no labeller logic; it only pins the
// org reusables and grants scopes. These assertions pin that contract so
// an accidental unpin or scope change fails in CI instead of silently
// weakening the required gate.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readPRSizeCaller(t *testing.T) string {
	t.Helper()
	// go test runs with the package directory (repo root) as cwd.
	raw, err := os.ReadFile(".github/workflows/pr-size.yml")
	if err != nil {
		t.Fatalf("read pr-size caller: %v", err)
	}
	return string(raw)
}

var pinnedUses = regexp.MustCompile(`uses: TogetherWeOwn/\.github/\.github/workflows/pr-size(?:-(?:gate|weekly))?\.yml@([0-9a-f]{40})`)

var allOrgUses = regexp.MustCompile(`uses:\s*TogetherWeOwn/\.github/`)

func TestPRSizeCallerPinsOrgReusables(t *testing.T) {
	body := readPRSizeCaller(t)
	matches := pinnedUses.FindAllStringSubmatch(body, -1)
	if len(matches) != 3 {
		t.Fatalf("want 3 pinned reusable calls, got %d", len(matches))
	}
	// Every org reusable reference must be pinned: an extra unpinned or
	// floating uses: TogetherWeOwn/.github call would keep the pinned
	// count at 3 but fail this check.
	all := allOrgUses.FindAllString(body, -1)
	if len(all) != len(matches) {
		t.Fatalf("every org reusable uses: must be pinned to a SHA: %d org uses: vs %d pinned", len(all), len(matches))
	}
	// All three calls must share one pinned SHA: no floating ref, no
	// per-call pin drift, no local script copy.
	sha := matches[0][1]
	for _, m := range matches[1:] {
		if m[1] != sha {
			t.Fatalf("pin drift: %q vs %q", sha, m[1])
		}
	}
	// Counting matches alone would pass if one job called another
	// job's reusable (e.g. the gate job calling pr-size.yml), so check
	// each job's uses: value is exactly its own reusable at the shared
	// SHA.
	for job, want := range map[string]string{
		"pr-size":        "pr-size.yml",
		"pr-size-gate":   "pr-size-gate.yml",
		"pr-size-weekly": "pr-size-weekly.yml",
	} {
		wantUses := "TogetherWeOwn/.github/.github/workflows/" + want + "@" + sha
		if got := jobUses(body, job); got != wantUses {
			t.Fatalf("job %q uses %q, want %q", job, got, wantUses)
		}
	}
	if _, err := os.Stat(".github/scripts/pr_size.py"); err == nil {
		t.Fatal("caller must not carry a local copy of the labeller script")
	}
}

// jobUses returns the `uses:` value of one caller job, or "" when the
// job or its uses: line is missing.
func jobUses(body, job string) string {
	lines := strings.Split(body, "\n")
	inJob := false
	for _, line := range lines {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":") {
			inJob = strings.TrimSpace(line) == job+":"
			continue
		}
		if inJob && strings.HasPrefix(line, "    uses: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "    uses: "))
		}
	}
	return ""
}

// jobPermissions extracts the permission map of one caller job by
// scanning for the job header and its `permissions:` block. It returns
// nil when the job or its permissions block is missing.
func jobPermissions(body, job string) map[string]string {
	lines := strings.Split(body, "\n")
	inJob := false
	inPerms := false
	grants := map[string]string{}
	for _, line := range lines {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":") {
			inJob = strings.TrimSpace(line) == job+":"
			inPerms = false
			continue
		}
		if !inJob {
			continue
		}
		if line == "    permissions:" {
			inPerms = true
			continue
		}
		if inPerms {
			if strings.HasPrefix(line, "      ") {
				trimmed := strings.TrimSpace(line)
				if k, v, ok := strings.Cut(trimmed, ":"); ok {
					grants[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
				continue
			}
			inPerms = false
		}
	}
	if len(grants) == 0 {
		return nil
	}
	return grants
}

func assertGrants(t *testing.T, job string, body string, want map[string]string) {
	t.Helper()
	got := jobPermissions(body, job)
	if got == nil {
		t.Fatalf("job %q missing permissions grant (want %v)", job, want)
	}
	if len(got) != len(want) {
		t.Fatalf("job %q grants %v, want exact %v", job, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("job %q grant %s=%q, want %s=%q (all grants %v)", job, k, got[k], k, v, got)
		}
	}
}

func TestPRSizeCallerScopes(t *testing.T) {
	body := readPRSizeCaller(t)
	// The labeller and the required gate write back; the weekly tally
	// stays read-only. GitHub validates nested-job permissions against
	// the caller grant, so keep the grants minimal AND per-job: a write
	// grant in the file alone does not prove the weekly job is
	// read-only. Assert each job's exact scope set.
	write := map[string]string{
		"contents":      "read",
		"issues":        "write",
		"pull-requests": "write",
	}
	read := map[string]string{
		"contents":      "read",
		"issues":        "read",
		"pull-requests": "read",
	}
	assertGrants(t, "pr-size", body, write)
	assertGrants(t, "pr-size-gate", body, write)
	assertGrants(t, "pr-size-weekly", body, read)
}

// triggerTypes returns the activity type set of one `on:` event block.
// A substring search would pass when `labeled` is dropped but
// `unlabeled` remains, so each event's `types:` list is parsed and
// compared by membership.
func triggerTypes(body, event string) map[string]bool {
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(event) + `:\n\s+types: \[([^\]]*)\]`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	types := map[string]bool{}
	for _, name := range strings.Split(m[1], ",") {
		types[strings.TrimSpace(name)] = true
	}
	return types
}

func TestPRSizeCallerGateTriggers(t *testing.T) {
	body := readPRSizeCaller(t)
	// Exception label changes and review submission/dismissal must
	// re-evaluate the gate without a manual re-run.
	want := map[string][]string{
		"pull_request":        {"opened", "synchronize", "reopened", "ready_for_review", "labeled", "unlabeled", "edited"},
		"pull_request_review": {"submitted", "dismissed"},
	}
	for event, types := range want {
		got := triggerTypes(body, event)
		if got == nil {
			t.Fatalf("caller must keep %s trigger types %v", event, types)
		}
		for _, typ := range types {
			if !got[typ] {
				t.Fatalf("caller must keep %s trigger type %q (got %v)", event, typ, got)
			}
		}
	}
	// A manual dispatch must not displace a pending PR-associated run in
	// the shared concurrency group, and a retargeted PR must re-run: the
	// concurrency group separates workflow_dispatch runs with a
	// `-dispatch` suffix. Without it, a dispatch can cancel a pending
	// PR run and leave a stale green check on an ungated diff.
	if !strings.Contains(body, "'-dispatch'") {
		t.Fatal("caller concurrency must separate workflow_dispatch runs with a -dispatch suffix")
	}
	if !strings.Contains(body, "does not refresh required checks") {
		t.Fatal("caller dispatch description must state it does not refresh required checks")
	}
	if !strings.Contains(body, "exclude-extra:") {
		t.Fatal("caller must keep exclude-extra inputs")
	}
	// No generated paths are committed in this repo on top of the
	// script's built-in lockfile/generated/fixture exclusions, so
	// exclude-extra stays empty; updating it means updating this test.
	excludes := regexp.MustCompile(`(?m)^      exclude-extra: "([^"]*)"`).FindAllStringSubmatch(body, -1)
	if len(excludes) != 2 {
		t.Fatalf("want exclude-extra on the label and gate jobs, got %d", len(excludes))
	}
	for _, m := range excludes {
		if m[1] != "" {
			t.Fatalf("unexpected exclude-extra %q", m[1])
		}
	}
}
