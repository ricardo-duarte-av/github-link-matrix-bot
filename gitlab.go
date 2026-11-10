package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// GitLab: https://docs.gitlab.com/api/

func (f *Forge) gitlabProjectAPI(repo string) string {
	return f.BaseURL + "/api/v4/projects/" + url.PathEscape(repo)
}

type gitlabRelease struct {
	Name        string    `json:"name"`
	TagName     string    `json:"tag_name"`
	Description string    `json:"description"`
	ReleasedAt  time.Time `json:"released_at"`
	Upcoming    bool      `json:"upcoming_release"`
	Author      *struct {
		Username string `json:"username"`
		WebURL   string `json:"web_url"`
	} `json:"author"`
	Assets struct {
		Sources []struct {
			Format string `json:"format"`
			URL    string `json:"url"`
		} `json:"sources"`
		Links []struct {
			Name           string `json:"name"`
			URL            string `json:"url"`
			DirectAssetURL string `json:"direct_asset_url"`
		} `json:"links"`
	} `json:"assets"`
	Links struct {
		Self string `json:"self"`
	} `json:"_links"`
}

func (fe *Fetcher) gitlabRelease(ctx context.Context, l Link) (*Release, error) {
	var g gitlabRelease
	// The tag must be escaped as a single path segment (slashes become %2F).
	_, err := fe.get(ctx, l.Forge, l.Forge.gitlabProjectAPI(l.Repo)+"/releases/"+url.PathEscape(l.Ref), &g)
	if err != nil {
		return nil, err
	}
	r := &Release{
		Name:        g.Name,
		TagName:     g.TagName,
		Body:        g.Description,
		HTMLURL:     g.Links.Self,
		Prerelease:  g.Upcoming,
		PublishedAt: g.ReleasedAt,
	}
	if r.HTMLURL == "" {
		r.HTMLURL = l.Forge.ReleaseURL(l.Repo, g.TagName)
	}
	if g.Author != nil {
		r.Author = User{Login: g.Author.Username, HTMLURL: g.Author.WebURL}
	}
	for _, as := range g.Assets.Links {
		u := as.DirectAssetURL
		if u == "" {
			u = as.URL
		}
		r.Assets = append(r.Assets, Asset{Name: as.Name, URL: u})
	}
	for _, s := range g.Assets.Sources {
		r.Sources = append(r.Sources, Asset{Name: "Source code (" + s.Format + ")", URL: s.URL})
	}
	return r, nil
}

type gitlabCommit struct {
	ID           string    `json:"id"`
	Message      string    `json:"message"`
	AuthorName   string    `json:"author_name"`
	AuthoredDate time.Time `json:"authored_date"`
	ParentIDs    []string  `json:"parent_ids"`
	WebURL       string    `json:"web_url"`
	Stats        struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	} `json:"stats"`
}

type gitlabDiff struct {
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
}

func (fe *Fetcher) gitlabCommit(ctx context.Context, l Link) (*Commit, error) {
	api := l.Forge.gitlabProjectAPI(l.Repo) + "/repository/commits/" + l.Ref
	var g gitlabCommit
	if _, err := fe.get(ctx, l.Forge, api+"?stats=true", &g); err != nil {
		return nil, err
	}
	c := &Commit{SHA: g.ID, HTMLURL: g.WebURL, NoFileStats: true}
	if c.HTMLURL == "" {
		c.HTMLURL = l.Forge.CommitURL(l.Repo, g.ID)
	}
	c.Commit.Message = g.Message
	c.Commit.Author.Name = g.AuthorName
	c.Commit.Author.Date = g.AuthoredDate
	c.Stats.Additions, c.Stats.Deletions = g.Stats.Additions, g.Stats.Deletions
	for _, p := range g.ParentIDs {
		c.Parents = append(c.Parents, CommitParent{SHA: p, HTMLURL: l.Forge.CommitURL(l.Repo, p)})
	}

	// The diff endpoint includes the diff text, so only fetch one page with
	// as many files as we show, and take the total from the headers.
	var diffs []gitlabDiff
	hdr, err := fe.get(ctx, l.Forge, fmt.Sprintf("%s/diff?per_page=%d", api, maxFiles), &diffs)
	if err != nil {
		return nil, err
	}
	for _, d := range diffs {
		status := "modified"
		switch {
		case d.NewFile:
			status = "added"
		case d.DeletedFile:
			status = "removed"
		case d.RenamedFile:
			status = "renamed"
		}
		c.Files = append(c.Files, CommitFile{Filename: d.NewPath, Status: status})
	}
	if n, err := strconv.Atoi(hdr.Get("X-Total")); err == nil {
		c.FileCount = n
	} else if hdr.Get("X-Next-Page") != "" {
		c.FileCount = -1 // more than shown; GitLab omits the total for huge diffs
	}
	return c, nil
}
