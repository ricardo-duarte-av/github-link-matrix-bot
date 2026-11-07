package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Link is a GitHub commit or release link found in a message.
type Link struct {
	Kind  string // "commit" or "release"
	Owner string
	Repo  string
	Ref   string // commit SHA or (unescaped) release tag
	URL   string
}

var linkRe = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/(commit/[0-9a-fA-F]{7,40}|releases/tag/[^\s"'<>?#]+)`)

func findLinks(texts ...string) []Link {
	var links []Link
	seen := map[string]bool{}
	for _, text := range texts {
		for _, m := range linkRe.FindAllStringSubmatch(text, -1) {
			l := Link{Owner: m[1], Repo: m[2]}
			if sha, ok := strings.CutPrefix(m[3], "commit/"); ok {
				l.Kind, l.Ref = "commit", strings.ToLower(sha)
			} else {
				tag := strings.TrimRight(strings.TrimPrefix(m[3], "releases/tag/"), ").,;")
				if unescaped, err := url.PathUnescape(tag); err == nil {
					tag = unescaped
				}
				l.Kind, l.Ref = "release", tag
			}
			key := strings.ToLower(l.Kind + "|" + l.Owner + "/" + l.Repo + "|" + l.Ref)
			if seen[key] {
				continue
			}
			seen[key] = true
			if l.Kind == "commit" {
				l.URL = fmt.Sprintf("https://github.com/%s/%s/commit/%s", l.Owner, l.Repo, l.Ref)
			} else {
				l.URL = fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", l.Owner, l.Repo, escapeTag(l.Ref))
			}
			links = append(links, l)
		}
	}
	return links
}

// escapeTag escapes each path segment of a tag (tags may contain slashes).
func escapeTag(tag string) string {
	parts := strings.Split(tag, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

type GitHub struct {
	Token  string
	Client *http.Client
}

type User struct {
	Login   string `json:"login"`
	HTMLURL string `json:"html_url"`
}

type Asset struct {
	Name          string    `json:"name"`
	Size          int64     `json:"size"`
	Digest        string    `json:"digest"`
	DownloadCount int       `json:"download_count"`
	URL           string    `json:"browser_download_url"`
	CreatedAt     time.Time `json:"created_at"`
}

type Release struct {
	Name        string    `json:"name"`
	TagName     string    `json:"tag_name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Author      User      `json:"author"`
	Assets      []Asset   `json:"assets"`
	ZipballURL  string    `json:"zipball_url"`
	TarballURL  string    `json:"tarball_url"`
}

type Commit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Author  *User `json:"author"`
	Parents []struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
	} `json:"parents"`
	Stats struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	} `json:"stats"`
	Files []struct {
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	} `json:"files"`
}

func (g *GitHub) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "github-link-matrix-bot")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *GitHub) Release(ctx context.Context, owner, repo, tag string) (*Release, error) {
	var r Release
	err := g.get(ctx, fmt.Sprintf("/repos/%s/%s/releases/tags/%s", owner, repo, escapeTag(tag)), &r)
	return &r, err
}

func (g *GitHub) Commit(ctx context.Context, owner, repo, sha string) (*Commit, error) {
	var c Commit
	err := g.get(ctx, fmt.Sprintf("/repos/%s/%s/commits/%s", owner, repo, sha), &c)
	return &c, err
}
