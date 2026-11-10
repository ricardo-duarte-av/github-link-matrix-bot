package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix/event"
)

// Forge kinds.
const (
	KindGitHub  = "github"
	KindForgejo = "forgejo" // also Gitea, whose API is the same
	KindGitLab  = "gitlab"
)

// Forge is a code hosting site the bot knows how to query.
type Forge struct {
	Kind    string
	BaseURL string // e.g. https://github.com, no trailing slash
	Token   string
	re      *regexp.Regexp
}

// defaultForges are always available; configured forges with the same URL
// replace them (e.g. to add a token).
func defaultForges(githubToken string) []ForgeConfig {
	return []ForgeConfig{
		{URL: "https://github.com", Type: KindGitHub, Token: githubToken},
		{URL: "https://gitlab.com", Type: KindGitLab},
		{URL: "https://codeberg.org", Type: KindForgejo},
	}
}

// NewForge validates a forge config entry.
func NewForge(fc ForgeConfig) (*Forge, error) {
	kind := strings.ToLower(fc.Type)
	if kind == "gitea" {
		kind = KindForgejo
	}
	switch kind {
	case KindGitHub, KindForgejo, KindGitLab:
	default:
		return nil, fmt.Errorf("forge %s: unknown type %q (want github, forgejo, gitea or gitlab)", fc.URL, fc.Type)
	}
	u, err := url.Parse(strings.TrimRight(fc.URL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("forge url %q must look like https://host[/path]", fc.URL)
	}
	f := &Forge{Kind: kind, BaseURL: u.Scheme + "://" + u.Host + u.EscapedPath(), Token: fc.Token}

	// Links may use either scheme; the host is case-insensitive.
	prefix := `https?://(?i:` + regexp.QuoteMeta(u.Host) + `)` + regexp.QuoteMeta(u.EscapedPath())
	const seg = `[A-Za-z0-9_.-]+`
	const tag = `[^\s"'<>?#]+`
	switch kind {
	case KindGitLab:
		// Projects can be nested in subgroups: group/sub/project/-/commit/sha.
		f.re = regexp.MustCompile(prefix + `/((?:` + seg + `/)+?` + seg + `)/-/(commit/[0-9a-fA-F]{7,40}|releases/` + tag + `)`)
	default:
		f.re = regexp.MustCompile(prefix + `/(` + seg + `/` + seg + `)/(commit/[0-9a-fA-F]{7,40}|releases/tag/` + tag + `)`)
	}
	return f, nil
}

// NewForges builds the forge list: the defaults plus the configured ones.
func NewForges(githubToken string, configured []ForgeConfig) ([]*Forge, error) {
	all := defaultForges(githubToken)
	for _, fc := range configured {
		replaced := false
		for i := range all {
			if strings.EqualFold(strings.TrimRight(all[i].URL, "/"), strings.TrimRight(fc.URL, "/")) {
				if fc.Token == "" && strings.EqualFold(fc.Type, all[i].Type) {
					fc.Token = all[i].Token
				}
				all[i], replaced = fc, true
			}
		}
		if !replaced {
			all = append(all, fc)
		}
	}
	forges := make([]*Forge, 0, len(all))
	for _, fc := range all {
		f, err := NewForge(fc)
		if err != nil {
			return nil, err
		}
		forges = append(forges, f)
	}
	return forges, nil
}

// Host returns the forge's host name, for logging.
func (f *Forge) Host() string {
	u, _ := url.Parse(f.BaseURL)
	return u.Host
}

func (f *Forge) RepoURL(repo string) string { return f.BaseURL + "/" + repo }

// web returns the path separator before repo sub-pages ("/-" on GitLab).
func (f *Forge) web() string {
	if f.Kind == KindGitLab {
		return "/-"
	}
	return ""
}

func (f *Forge) CommitURL(repo, sha string) string {
	return f.RepoURL(repo) + f.web() + "/commit/" + sha
}

func (f *Forge) ReleaseURL(repo, tag string) string {
	if f.Kind == KindGitLab {
		return f.RepoURL(repo) + "/-/releases/" + url.PathEscape(tag)
	}
	return f.RepoURL(repo) + "/releases/tag/" + escapeTag(tag)
}

func (f *Forge) IssueURL(repo, n string) string {
	return f.RepoURL(repo) + f.web() + "/issues/" + n
}

func (f *Forge) MergeRequestURL(repo, n string) string {
	return f.RepoURL(repo) + "/-/merge_requests/" + n
}

func (f *Forge) TreeURL(repo, branch string) string {
	switch f.Kind {
	case KindForgejo:
		return f.RepoURL(repo) + "/src/branch/" + escapeTag(branch)
	default:
		return f.RepoURL(repo) + f.web() + "/tree/" + escapeTag(branch)
	}
}

// CommitsURL links to the commit history of branch (or the default branch).
func (f *Forge) CommitsURL(repo, branch string) string {
	switch f.Kind {
	case KindForgejo:
		if branch == "" {
			return f.RepoURL(repo) // Forgejo has no branch-less history page
		}
		return f.RepoURL(repo) + "/commits/branch/" + escapeTag(branch)
	default:
		u := f.RepoURL(repo) + f.web() + "/commits"
		if branch != "" {
			u += "/" + escapeTag(branch)
		}
		return u
	}
}

// Link is a commit or release link found in a message.
type Link struct {
	Forge *Forge
	Kind  string // "commit" or "release"
	Repo  string // owner/repo, or group/subgroup/project on GitLab
	Ref   string // commit SHA or (unescaped) release tag
	URL   string
}

func (l Link) RepoURL() string { return l.Forge.RepoURL(l.Repo) }

func findLinks(forges []*Forge, texts ...string) []Link {
	var links []Link
	seen := map[string]bool{}
	for _, text := range texts {
		for _, f := range forges {
			for _, m := range f.re.FindAllStringSubmatch(text, -1) {
				l := Link{Forge: f, Repo: m[1]}
				if sha, ok := strings.CutPrefix(m[2], "commit/"); ok {
					l.Kind, l.Ref = "commit", strings.ToLower(sha)
				} else {
					tag := strings.TrimPrefix(m[2], "releases/")
					tag = strings.TrimPrefix(tag, "tag/")
					tag = strings.TrimRight(tag, ").,;")
					if f.Kind == KindGitLab && (tag == "permalink" || strings.HasPrefix(tag, "permalink/")) {
						continue // /-/releases/permalink/latest isn't a tag
					}
					if unescaped, err := url.PathUnescape(tag); err == nil {
						tag = unescaped
					}
					l.Kind, l.Ref = "release", tag
				}
				key := strings.ToLower(l.Kind + "|" + l.RepoURL() + "|" + l.Ref)
				if seen[key] {
					continue
				}
				seen[key] = true
				if l.Kind == "commit" {
					l.URL = f.CommitURL(l.Repo, l.Ref)
				} else {
					l.URL = f.ReleaseURL(l.Repo, l.Ref)
				}
				links = append(links, l)
			}
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

// User, Asset, Release and Commit are the common model all forges are
// converted to. Their JSON tags match the GitHub API (which Forgejo mostly
// follows); fields tagged "-" are filled in by the forge-specific code.
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
	// Source code archive links shown after the assets.
	Sources []Asset `json:"-"`
}

type CommitParent struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
}

type CommitFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
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
	Author  *User          `json:"author"`
	Parents []CommitParent `json:"parents"`
	Stats   struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	} `json:"stats"`
	Files []CommitFile `json:"files"`
	// NoFileStats is set when the forge doesn't report per-file +/- counts.
	NoFileStats bool `json:"-"`
	// FileCount is the number of changed files when Files is incomplete: 0
	// means len(Files), -1 that there are more files but the total is unknown.
	FileCount int `json:"-"`
}

// NumFiles returns the number of changed files, and false if it is only a
// lower bound.
func (c *Commit) NumFiles() (int, bool) {
	if c.FileCount < 0 {
		return len(c.Files), false
	}
	return max(c.FileCount, len(c.Files)), true
}

// HTTPError is returned for non-200 API responses.
type HTTPError struct {
	URL        string
	Status     string
	StatusCode int
	Body       string
	// RetryAfter is set when the request was rate limited: how long to wait
	// before trying again.
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("GET %s: %s: %s", e.URL, e.Status, e.Body)
}

func isNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.StatusCode == http.StatusNotFound
}

// rateLimitWait reports how long to wait if err is a rate-limit error.
func rateLimitWait(err error) (time.Duration, bool) {
	var he *HTTPError
	if errors.As(err, &he) && he.RetryAfter > 0 {
		return he.RetryAfter, true
	}
	return 0, false
}

const (
	// Used for secondary rate limits that don't say when to retry; GitHub's
	// docs ask to wait at least a minute.
	defaultRateLimitWait = time.Minute
	maxRateLimitWait     = time.Hour
)

// parseRateLimit returns how long to wait for a rate-limited response, or 0 if
// the response isn't a rate-limit error. See
// https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api
// GitLab answers 429 with Retry-After, which is handled the same way.
func parseRateLimit(resp *http.Response, body string, now time.Time) time.Duration {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	var wait time.Duration
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil {
			wait = time.Duration(secs) * time.Second
		}
	} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			// Small buffer for clock skew between us and GitHub.
			wait = time.Unix(reset, 0).Sub(now) + 2*time.Second
		}
	} else if resp.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(body), "rate limit") {
		wait = defaultRateLimitWait
	} else {
		return 0 // a plain 403 (permissions etc.)
	}
	return min(max(wait, time.Second), maxRateLimitWait)
}

// Fetcher finds links to the known forges and queries their APIs.
type Fetcher struct {
	Client *http.Client
	Forges []*Forge
}

// get fetches an API URL and decodes the JSON response into out.
func (fe *Fetcher) get(ctx context.Context, f *Forge, apiURL string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "github-link-matrix-bot")
	switch f.Kind {
	case KindGitHub:
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if f.Token != "" {
			req.Header.Set("Authorization", "Bearer "+f.Token)
		}
	case KindForgejo:
		if f.Token != "" {
			req.Header.Set("Authorization", "token "+f.Token)
		}
	case KindGitLab:
		if f.Token != "" {
			req.Header.Set("PRIVATE-TOKEN", f.Token)
		}
	}
	resp, err := fe.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		body := strings.TrimSpace(string(raw))
		return nil, &HTTPError{
			URL:        apiURL,
			Status:     resp.Status,
			StatusCode: resp.StatusCode,
			Body:       body,
			RetryAfter: parseRateLimit(resp, body, time.Now()),
		}
	}
	return resp.Header, json.NewDecoder(resp.Body).Decode(out)
}

func (fe *Fetcher) Release(ctx context.Context, l Link) (*Release, error) {
	switch l.Forge.Kind {
	case KindForgejo:
		return fe.forgejoRelease(ctx, l)
	case KindGitLab:
		return fe.gitlabRelease(ctx, l)
	default:
		return fe.githubRelease(ctx, l)
	}
}

func (fe *Fetcher) Commit(ctx context.Context, l Link) (*Commit, error) {
	switch l.Forge.Kind {
	case KindForgejo:
		return fe.forgejoCommit(ctx, l)
	case KindGitLab:
		return fe.gitlabCommit(ctx, l)
	default:
		return fe.githubCommit(ctx, l)
	}
}

// Fetch retrieves the commit or release a link points to and renders it. It
// also returns a short Entry for the daily summary.
func (fe *Fetcher) Fetch(ctx context.Context, l Link, branch string) (*event.MessageEventContent, *Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	e := &Entry{Kind: l.Kind, Forge: l.Forge.Kind, Repo: l.Repo, RepoURL: l.RepoURL(), Ref: l.Ref, URL: l.URL, Branch: branch}
	switch l.Kind {
	case "release":
		r, err := fe.Release(ctx, l)
		if err != nil {
			return nil, nil, err
		}
		e.Title, e.Ref, e.Prerelease = r.Name, r.TagName, r.Prerelease
		if e.Title == "" {
			e.Title = r.TagName
		}
		e.Author, e.AuthorURL = r.Author.Login, r.Author.HTMLURL
		return renderRelease(l, r), e, nil
	case "commit":
		c, err := fe.Commit(ctx, l)
		if err != nil {
			return nil, nil, err
		}
		e.Ref = c.SHA
		e.Title, _, _ = strings.Cut(strings.TrimSpace(strings.ReplaceAll(c.Commit.Message, "\r\n", "\n")), "\n")
		if c.Author != nil && c.Author.Login != "" {
			e.Author, e.AuthorURL = c.Author.Login, c.Author.HTMLURL
		} else {
			e.Author = c.Commit.Author.Name
		}
		return renderCommit(l, c, branch), e, nil
	}
	return nil, nil, fmt.Errorf("unknown link kind %q", l.Kind)
}
