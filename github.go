package main

import (
	"context"
	"fmt"
	"strings"
)

func (f *Forge) githubAPI() string {
	if strings.EqualFold(f.BaseURL, "https://github.com") {
		return "https://api.github.com"
	}
	return f.BaseURL + "/api/v3" // GitHub Enterprise Server
}

func (fe *Fetcher) githubRelease(ctx context.Context, l Link) (*Release, error) {
	var r Release
	_, err := fe.get(ctx, l.Forge, fmt.Sprintf("%s/repos/%s/releases/tags/%s", l.Forge.githubAPI(), l.Repo, escapeTag(l.Ref)), &r)
	if err != nil {
		return nil, err
	}
	// zipball_url/tarball_url point at the API; link the web archives instead.
	base := l.RepoURL() + "/archive/refs/tags/" + escapeTag(r.TagName)
	r.Sources = []Asset{
		{Name: "Source code (zip)", URL: base + ".zip"},
		{Name: "Source code (tar.gz)", URL: base + ".tar.gz"},
	}
	return &r, nil
}

func (fe *Fetcher) githubCommit(ctx context.Context, l Link) (*Commit, error) {
	var c Commit
	_, err := fe.get(ctx, l.Forge, fmt.Sprintf("%s/repos/%s/commits/%s", l.Forge.githubAPI(), l.Repo, l.Ref), &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}
