package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, loc)
	if got, want := nextRun(now, 23, 59, loc), time.Date(2026, 11, 10, 23, 59, 0, 0, loc); !got.Equal(want) {
		t.Errorf("same day: got %v, want %v", got, want)
	}
	now = time.Date(2026, 11, 10, 23, 59, 0, 0, loc)
	if got, want := nextRun(now, 23, 59, loc), time.Date(2026, 11, 11, 23, 59, 0, 0, loc); !got.Equal(want) {
		t.Errorf("at run time: got %v, want %v", got, want)
	}
}

func makeEntries(repos, commits, releases int) []Entry {
	day := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	var es []Entry
	for r := 0; r < repos; r++ {
		name := fmt.Sprintf("owner/repo%02d", r)
		for i := 0; i < releases; i++ {
			es = append(es, Entry{Kind: "release", Repo: name, Ref: fmt.Sprintf("v1.%d", i), Title: "Release " + strings.Repeat("x", 100),
				URL: fmt.Sprintf("https://github.com/%s/releases/tag/v1.%d", name, i), Author: "alice", AuthorURL: "https://github.com/alice", LoggedAt: day})
		}
		for i := 0; i < commits; i++ {
			sha := fmt.Sprintf("%040x", r*100000+i)
			es = append(es, Entry{Kind: "commit", Repo: name, Ref: sha, Title: "Fix #12 " + strings.Repeat("y", 300),
				URL: "https://github.com/" + name + "/commit/" + sha, Author: "bob", AuthorURL: "https://github.com/bob", LoggedAt: day})
		}
	}
	return es
}

func TestRenderSummarySingleMessage(t *testing.T) {
	msgs := renderSummary(makeEntries(2, 3, 1), time.UTC, 50, maxSummaryContentBytes)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].FormattedBody, "Daily summary — 2026-11-10") {
		t.Errorf("missing title: %s", msgs[0].FormattedBody)
	}
}

func TestRenderSummarySplits(t *testing.T) {
	const limit = 8 * 1024
	entries := makeEntries(5, 40, 2) // one repo alone exceeds the limit
	msgs := renderSummary(entries, time.UTC, -1, limit)
	if len(msgs) < 2 {
		t.Fatalf("expected a split, got %d message(s)", len(msgs))
	}
	var all strings.Builder
	for i, m := range msgs {
		if n := contentSize(m.FormattedBody); n > limit {
			t.Errorf("message %d is %d bytes, over %d", i, n, limit)
		}
		if i > 0 && !strings.Contains(m.FormattedBody, "(continued)") {
			t.Errorf("message %d lacks continued header", i)
		}
		all.WriteString(m.FormattedBody)
	}
	for _, e := range entries {
		if !strings.Contains(all.String(), e.URL) {
			t.Errorf("entry %s missing from summary", e.URL)
		}
	}
}

func TestRenderSummaryCap(t *testing.T) {
	msgs := renderSummary(makeEntries(1, 10, 0), time.UTC, 4, maxSummaryContentBytes)
	body := msgs[0].FormattedBody
	if strings.Count(body, "/commit/") != 4 || !strings.Contains(body, "and 6 more") {
		t.Errorf("cap not applied: %s", body)
	}
}
