package main

import (
	"context"
	"fmt"
)

// Forgejo and Gitea: https://forgejo.org/docs/latest/user/api-usage/
// The API mirrors GitHub's closely, so it decodes into the same structs.

func (f *Forge) forgejoAPI() string { return f.BaseURL + "/api/v1" }

func (fe *Fetcher) forgejoRelease(ctx context.Context, l Link) (*Release, error) {
	var r struct {
		Release
		HideArchiveLinks bool `json:"hide_archive_links"`
	}
	_, err := fe.get(ctx, l.Forge, fmt.Sprintf("%s/repos/%s/releases/tags/%s", l.Forge.forgejoAPI(), l.Repo, escapeTag(l.Ref)), &r)
	if err != nil {
		return nil, err
	}
	// Here zipball_url/tarball_url are web links already.
	if !r.HideArchiveLinks {
		if r.ZipballURL != "" {
			r.Sources = append(r.Sources, Asset{Name: "Source code (zip)", URL: r.ZipballURL})
		}
		if r.TarballURL != "" {
			r.Sources = append(r.Sources, Asset{Name: "Source code (tar.gz)", URL: r.TarballURL})
		}
	}
	return &r.Release, nil
}

func (fe *Fetcher) forgejoCommit(ctx context.Context, l Link) (*Commit, error) {
	var c Commit
	_, err := fe.get(ctx, l.Forge, fmt.Sprintf("%s/repos/%s/git/commits/%s?stat=true&files=true&verification=false",
		l.Forge.forgejoAPI(), l.Repo, l.Ref), &c)
	if err != nil {
		return nil, err
	}
	// Parents only carry an API url, and files have no per-file stats.
	for i := range c.Parents {
		c.Parents[i].HTMLURL = l.Forge.CommitURL(l.Repo, c.Parents[i].SHA)
	}
	c.NoFileStats = true
	return &c, nil
}
