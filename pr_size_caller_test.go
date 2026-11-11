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

func TestPRSizeCallerPinsOrgReusables(t *testing.T) {
	body := readPRSizeCaller(t)
	matches := pinnedUses.FindAllStringSubmatch(body, -1)
	if len(matches) != 3 {
		t.Fatalf("want 3 pinned reusable calls, got %d", len(matches))
	}
	// All three calls must share one pinned SHA: no floating ref, no
	// per-call pin drift, no local script copy.
	sha := matches[0][1]
	for _, m := range matches[1:] {
		if m[1] != sha {
			t.Fatalf("pin drift: %q vs %q", sha, m[1])
		}
	}
	if _, err := os.Stat(".github/scripts/pr_size.py"); err == nil {
		t.Fatal("caller must not carry a local copy of the labeller script")
	}
}

func TestPRSizeCallerScopes(t *testing.T) {
	body := readPRSizeCaller(t)
	// The labeller and the required gate write back; the weekly tally
	// stays read-only. GitHub validates nested-job permissions against
	// the caller grant, so keep the grants minimal.
	for _, want := range []string{"issues: write", "pull-requests: write"} {
		if !strings.Contains(body, want) {
			t.Fatalf("caller jobs must grant %q", want)
		}
	}
	for _, job := range []string{"  pr-size:", "  pr-size-gate:", "  pr-size-weekly:"} {
		if !strings.Contains(body, job) {
			t.Fatalf("caller must keep job %q", job)
		}
	}
}

func TestPRSizeCallerGateTriggers(t *testing.T) {
	body := readPRSizeCaller(t)
	// Exception label changes and review submission/dismissal must
	// re-evaluate the gate without a manual re-run.
	for _, want := range []string{
		"labeled",
		"unlabeled",
		"pull_request_review:",
		"exclude-extra:",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("caller must keep gate trigger %q", want)
		}
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
