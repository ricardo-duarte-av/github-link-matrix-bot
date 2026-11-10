package main

import (
	"strings"
	"testing"
	"time"
)

func testForges(t *testing.T) []*Forge {
	t.Helper()
	fs, err := NewForges("", []ForgeConfig{
		{URL: "https://git.eden-emu.dev/", Type: "forgejo"},
		{URL: "https://git.example.org/gitlab", Type: "gitlab"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestFindLinks(t *testing.T) {
	fs := testForges(t)
	tests := []struct {
		in                   string
		kind, repo, ref, url string
		forgeKind            string
	}{
		{"see https://github.com/o/r/commit/ABCDEF1.", "commit", "o/r", "abcdef1", "https://github.com/o/r/commit/abcdef1", KindGitHub},
		{"https://github.com/o/r/releases/tag/v1%2Fx)", "release", "o/r", "v1/x", "https://github.com/o/r/releases/tag/v1/x", KindGitHub},
		{"https://git.eden-emu.dev/eden-emu/eden/commit/89d1a1cd70", "commit", "eden-emu/eden", "89d1a1cd70", "https://git.eden-emu.dev/eden-emu/eden/commit/89d1a1cd70", KindForgejo},
		{"http://GIT.EDEN-EMU.DEV/eden-emu/eden/releases/tag/v0.2.1", "release", "eden-emu/eden", "v0.2.1", "https://git.eden-emu.dev/eden-emu/eden/releases/tag/v0.2.1", KindForgejo},
		{"https://codeberg.org/a/b/commit/1234567", "commit", "a/b", "1234567", "https://codeberg.org/a/b/commit/1234567", KindForgejo},
		{"https://gitlab.com/g/sub/p/-/commit/44feccdf", "commit", "g/sub/p", "44feccdf", "https://gitlab.com/g/sub/p/-/commit/44feccdf", KindGitLab},
		{"https://gitlab.com/g/p/-/releases/v1%2F2", "release", "g/p", "v1/2", "https://gitlab.com/g/p/-/releases/v1%2F2", KindGitLab},
		{"https://git.example.org/gitlab/g/p/-/commit/abcdef0", "commit", "g/p", "abcdef0", "https://git.example.org/gitlab/g/p/-/commit/abcdef0", KindGitLab},
	}
	for _, tt := range tests {
		links := findLinks(fs, tt.in)
		if len(links) != 1 {
			t.Errorf("%s: got %d links, want 1", tt.in, len(links))
			continue
		}
		l := links[0]
		if l.Kind != tt.kind || l.Repo != tt.repo || l.Ref != tt.ref || l.URL != tt.url || l.Forge.Kind != tt.forgeKind {
			t.Errorf("%s: got %+v (forge %s)", tt.in, l, l.Forge.Kind)
		}
	}

	for _, in := range []string{
		"https://git.eden-emu.dev/eden-emu/eden/pulls/4691",
		"https://unknown.example/o/r/commit/abcdef1",
		"https://gitlab.com/g/p/-/releases/permalink/latest",
		"https://github.com/o/r/-/commit/abcdef1x",
	} {
		if links := findLinks(fs, in); len(links) != 0 {
			t.Errorf("%s: expected no links, got %+v", in, links)
		}
	}

	// The same link in body and formatted body is reported once.
	if n := len(findLinks(fs, "https://github.com/o/r/commit/abcdef1", `<a href="https://github.com/o/r/commit/ABCDEF1">x</a>`)); n != 1 {
		t.Errorf("dedup: got %d links", n)
	}
}

func TestNewForgesValidation(t *testing.T) {
	for _, fc := range []ForgeConfig{
		{URL: "https://x.org", Type: "svn"},
		{URL: "x.org", Type: "gitlab"},
	} {
		if _, err := NewForges("", []ForgeConfig{fc}); err == nil {
			t.Errorf("%+v: expected error", fc)
		}
	}
	// Overriding a default keeps the GitHub token unless a new one is given.
	fs, err := NewForges("tok", []ForgeConfig{{URL: "https://github.com/", Type: "github"}})
	if err != nil || fs[0].Token != "tok" || len(fs) != 3 {
		t.Errorf("override: %v %+v", err, fs)
	}
}

func TestLinkifyRefsPerForge(t *testing.T) {
	fs := testForges(t)
	gl := findLinks(fs, "https://gitlab.com/g/p/-/commit/abcdef1")[0]
	got := linkifyRefs("Fix #3, see !4 and 1a2b3c4d", gl.Forge, gl.Repo, false)
	for _, want := range []string{"https://gitlab.com/g/p/-/issues/3", "https://gitlab.com/g/p/-/merge_requests/4", "https://gitlab.com/g/p/-/commit/1a2b3c4d"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	fj := findLinks(fs, "https://git.eden-emu.dev/e/e/commit/abcdef1")[0]
	if got := linkifyRefs("(#4689) !5", fj.Forge, fj.Repo, false); !strings.Contains(got, "https://git.eden-emu.dev/e/e/issues/4689") || strings.Contains(got, "merge_requests") {
		t.Errorf("forgejo: %s", got)
	}
}

func TestSummaryMixedForges(t *testing.T) {
	day := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	entries := []Entry{
		// An entry from an older state file, without forge/repo_url.
		{Kind: "commit", Repo: "eden-emu/eden", Ref: strings.Repeat("a", 40), URL: "https://github.com/eden-emu/eden/commit/aaa", Title: "gh #1", LoggedAt: day},
		{Kind: "commit", Forge: KindForgejo, Repo: "eden-emu/eden", RepoURL: "https://git.eden-emu.dev/eden-emu/eden", Ref: strings.Repeat("b", 40),
			URL: "https://git.eden-emu.dev/eden-emu/eden/commit/bbb", Title: "fj #2", Branch: "master", LoggedAt: day},
		{Kind: "commit", Forge: KindForgejo, Repo: "eden-emu/eden", RepoURL: "https://git.eden-emu.dev/eden-emu/eden", Ref: strings.Repeat("c", 40),
			URL: "https://git.eden-emu.dev/eden-emu/eden/commit/ccc", Title: "fj #3", Branch: "master", LoggedAt: day},
	}
	body := renderSummary(entries, time.UTC, 1, maxSummaryContentBytes)[0].FormattedBody
	for _, want := range []string{
		`<a href="https://github.com/eden-emu/eden">`,
		`<a href="https://git.eden-emu.dev/eden-emu/eden">`,
		"https://github.com/eden-emu/eden/issues/1",
		"https://git.eden-emu.dev/eden-emu/eden/issues/2",
		"https://git.eden-emu.dev/eden-emu/eden/commits/branch/master",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in summary:\n%s", want, body)
		}
	}
	if strings.Count(body, "📁") != 2 {
		t.Errorf("same path on two forges should be two groups:\n%s", body)
	}
}
