package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Matrix events are limited to 64 KiB including the event envelope (sender,
// signatures, hashes, ...), and Go's JSON encoding escapes < and > as <.
// Keep the encoded content well below that.
const maxSummaryContentBytes = 48 * 1024

const maxSummaryTitle = 200

// Entry is a commit or release the bot posted info about, kept for the daily summary.
type Entry struct {
	Kind       string    `json:"kind"`               // "commit" or "release"
	Forge      string    `json:"forge,omitempty"`    // forge kind; empty means github (older state files)
	Repo       string    `json:"repo"`               // owner/repo (group/.../project on GitLab)
	RepoURL    string    `json:"repo_url,omitempty"` // empty means https://github.com/<repo>
	Ref        string    `json:"ref"`                // commit SHA or release tag
	URL        string    `json:"url"`
	Title      string    `json:"title"` // commit subject or release name
	Author     string    `json:"author,omitempty"`
	AuthorURL  string    `json:"author_url,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Prerelease bool      `json:"prerelease,omitempty"`
	LoggedAt   time.Time `json:"logged_at"`
}

// forge returns a Forge for building links to the entry's repo.
func (e Entry) forge() (*Forge, string) {
	repoURL := e.RepoURL
	if repoURL == "" {
		repoURL = "https://github.com/" + e.Repo
	}
	kind := e.Forge
	if kind == "" {
		kind = KindGitHub
	}
	base := strings.TrimSuffix(repoURL, "/"+e.Repo)
	return &Forge{Kind: kind, BaseURL: base}, repoURL
}

// Summary collects entries per room and posts a digest once a day.
type Summary struct {
	cfg  SummaryConfig
	loc  *time.Location
	hour int
	min  int
	log  zerolog.Logger

	mu    sync.Mutex
	rooms map[id.RoomID][]Entry
}

type summaryState struct {
	Rooms map[id.RoomID][]Entry `json:"rooms"`
}

func NewSummary(cfg SummaryConfig, log zerolog.Logger) (*Summary, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("summary timezone: %w", err)
	}
	t, err := time.Parse("15:04", cfg.Time)
	if err != nil {
		return nil, fmt.Errorf("summary time %q (want HH:MM): %w", cfg.Time, err)
	}
	s := &Summary{cfg: cfg, loc: loc, hour: t.Hour(), min: t.Minute(), log: log, rooms: map[id.RoomID][]Entry{}}
	if cfg.StateFile == "" {
		log.Warn().Msg("summary.state_file not set; the daily log is kept in memory only and lost on restart")
		return s, nil
	}
	data, err := os.ReadFile(cfg.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	} else if err != nil {
		return nil, fmt.Errorf("read summary state: %w", err)
	}
	var st summaryState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse summary state %s: %w", cfg.StateFile, err)
	}
	if st.Rooms != nil {
		s.rooms = st.Rooms
	}
	return s, nil
}

// Add logs an entry for a room, ignoring duplicates. Safe to call on a nil Summary.
func (s *Summary) Add(roomID id.RoomID, e Entry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.rooms[roomID] {
		if strings.EqualFold(old.URL, e.URL) {
			return
		}
	}
	if e.LoggedAt.IsZero() {
		e.LoggedAt = time.Now()
	}
	s.rooms[roomID] = append(s.rooms[roomID], e)
	s.saveLocked()
}

func (s *Summary) saveLocked() {
	if s.cfg.StateFile == "" {
		return
	}
	data, err := json.Marshal(summaryState{Rooms: s.rooms})
	if err != nil {
		s.log.Error().Err(err).Msg("Failed to encode summary state")
		return
	}
	tmp := s.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.log.Error().Err(err).Str("path", tmp).Msg("Failed to write summary state")
		return
	}
	if err := os.Rename(tmp, s.cfg.StateFile); err != nil {
		s.log.Error().Err(err).Str("path", s.cfg.StateFile).Msg("Failed to write summary state")
	}
}

func nextRun(now time.Time, hour, minute int, loc *time.Location) time.Time {
	n := now.In(loc)
	t := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
	if !t.After(n) {
		t = time.Date(n.Year(), n.Month(), n.Day()+1, hour, minute, 0, 0, loc)
	}
	return t
}

// Run posts the summaries every day at the configured time until ctx is done.
func (s *Summary) Run(ctx context.Context, client *mautrix.Client) {
	for {
		next := nextRun(time.Now(), s.hour, s.min, s.loc)
		s.log.Info().Time("at", next).Msg("Next daily summary scheduled")
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.sendAll(ctx, client)
	}
}

func (s *Summary) sendAll(ctx context.Context, client *mautrix.Client) {
	s.mu.Lock()
	snapshot := make(map[id.RoomID][]Entry, len(s.rooms))
	for room, entries := range s.rooms {
		if len(entries) > 0 {
			snapshot[room] = append([]Entry(nil), entries...)
		}
	}
	s.mu.Unlock()

	for room, entries := range snapshot {
		log := s.log.With().Stringer("room_id", room).Logger()
		msgs := renderSummary(entries, s.loc, s.cfg.MaxCommitsPerRepo, maxSummaryContentBytes)
		sent := true
		for i, content := range msgs {
			content.Mentions = &event.Mentions{}
			if _, err := client.SendMessageEvent(ctx, room, event.EventMessage, content); err != nil {
				// Keep the entries so they're included in the next summary.
				log.Error().Err(err).Int("part", i+1).Int("parts", len(msgs)).Msg("Failed to send daily summary")
				sent = false
				break
			}
		}
		if !sent {
			continue
		}
		log.Info().Int("entries", len(entries)).Int("messages", len(msgs)).Msg("Posted daily summary")
		s.mu.Lock()
		// Entries are only ever appended, so the first len(entries) are the ones just sent.
		if rest := s.rooms[room][len(entries):]; len(rest) > 0 {
			s.rooms[room] = rest
		} else {
			delete(s.rooms, room)
		}
		s.saveLocked()
		s.mu.Unlock()
	}
}

type repoGroup struct {
	name     string
	url      string
	forge    *Forge
	releases []Entry
	commits  []Entry
}

func groupByRepo(entries []Entry) []repoGroup {
	idx := map[string]int{}
	var groups []repoGroup
	for _, e := range entries {
		f, repoURL := e.forge()
		key := strings.ToLower(repoURL)
		i, ok := idx[key]
		if !ok {
			i = len(groups)
			idx[key] = i
			groups = append(groups, repoGroup{name: e.Repo, url: repoURL, forge: f})
		}
		if e.Kind == "release" {
			groups[i].releases = append(groups[i].releases, e)
		} else {
			groups[i].commits = append(groups[i].commits, e)
		}
	}
	sort.SliceStable(groups, func(a, b int) bool {
		na, nb := strings.ToLower(groups[a].name), strings.ToLower(groups[b].name)
		if na != nb {
			return na < nb
		}
		return strings.ToLower(groups[a].url) < strings.ToLower(groups[b].url)
	})
	return groups
}

// summaryDates returns the local date (or date range) the entries were logged in.
func summaryDates(entries []Entry, loc *time.Location) string {
	first, last := entries[0].LoggedAt, entries[0].LoggedAt
	for _, e := range entries[1:] {
		if e.LoggedAt.Before(first) {
			first = e.LoggedAt
		}
		if e.LoggedAt.After(last) {
			last = e.LoggedAt
		}
	}
	f, l := first.In(loc).Format("2006-01-02"), last.In(loc).Format("2006-01-02")
	if f == l {
		return f
	}
	return f + " – " + l
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func contentSize(htmlBody string) int {
	b, _ := json.Marshal(toContent(htmlBody))
	return len(b)
}

type summaryItem struct {
	release bool
	html    string
}

func repoItems(g repoGroup, maxCommits int) []summaryItem {
	var items []summaryItem
	for _, e := range g.releases {
		line := a(e.URL, truncateRunes(e.Title, maxSummaryTitle))
		if e.Ref != e.Title {
			line += " <code>" + html.EscapeString(e.Ref) + "</code>"
		}
		if e.Prerelease {
			line += " <b>pre-release</b>"
		}
		if e.Author != "" {
			line += " — by " + a(e.AuthorURL, e.Author)
		}
		items = append(items, summaryItem{release: true, html: "<li>" + line + "</li>\n"})
	}

	branches := map[string]bool{}
	for _, e := range g.commits {
		branches[e.Branch] = true
	}
	for i, e := range g.commits {
		if maxCommits > 0 && i == maxCommits {
			branch := ""
			if len(branches) == 1 {
				branch = e.Branch
			}
			more := g.forge.CommitsURL(g.name, branch)
			items = append(items, summaryItem{html: fmt.Sprintf("<li>… and %d more, see %s</li>\n", len(g.commits)-maxCommits, a(more, "the commit history"))})
			break
		}
		short := e.Ref[:min(7, len(e.Ref))]
		line := `<a href="` + html.EscapeString(e.URL) + `"><code>` + short + `</code></a> ` +
			linkifyRefs(html.EscapeString(truncateRunes(e.Title, maxSummaryTitle)), g.forge, g.name, false)
		if len(branches) > 1 && e.Branch != "" {
			line += " (" + html.EscapeString(e.Branch) + ")"
		}
		if e.Author != "" {
			if e.AuthorURL != "" {
				line += " — " + a(e.AuthorURL, e.Author)
			} else {
				line += " — " + html.EscapeString(e.Author)
			}
		}
		items = append(items, summaryItem{html: "<li>" + line + "</li>\n"})
	}
	return items
}

func renderRepoBlock(g repoGroup, items []summaryItem, cont bool) string {
	var sb strings.Builder
	header := "📁 " + a(g.url, g.name)
	if cont {
		header += " (continued)"
	}
	sb.WriteString("<h4>" + header + "</h4>\n")
	var rel, com []string
	for _, it := range items {
		if it.release {
			rel = append(rel, it.html)
		} else {
			com = append(com, it.html)
		}
	}
	if len(rel) > 0 {
		fmt.Fprintf(&sb, "<p><b>📦 Releases (%d)</b></p>\n<ul>\n%s</ul>\n", len(g.releases), strings.Join(rel, ""))
	}
	if len(com) > 0 {
		fmt.Fprintf(&sb, "<p><b>🔨 Commits (%d)</b></p>\n<ul>\n%s</ul>\n", len(g.commits), strings.Join(com, ""))
	}
	return sb.String()
}

// renderSummary builds the summary messages for one room, splitting them so
// each message's encoded content stays within limit bytes. Messages are split
// between repos; a repo too large for one message is split between its items.
func renderSummary(entries []Entry, loc *time.Location, maxCommits, limit int) []*event.MessageEventContent {
	if len(entries) == 0 {
		return nil
	}
	title := "📋 Daily summary — " + summaryDates(entries, loc)
	firstHead := "<h3>" + html.EscapeString(title) + "</h3>\n"
	contHead := "<h3>" + html.EscapeString(title) + " (continued)</h3>\n"
	fits := func(body string) bool { return contentSize(contHead+body) <= limit }

	var blocks []string
	for _, g := range groupByRepo(entries) {
		items := repoItems(g, maxCommits)
		if whole := renderRepoBlock(g, items, false); fits(whole) {
			blocks = append(blocks, whole)
			continue
		}
		for start := 0; start < len(items); {
			end := start + 1
			for end < len(items) && fits(renderRepoBlock(g, items[start:end+1], start > 0)) {
				end++
			}
			blocks = append(blocks, renderRepoBlock(g, items[start:end], start > 0))
			start = end
		}
	}

	var msgs []*event.MessageEventContent
	cur := ""
	flush := func() {
		head := firstHead
		if len(msgs) > 0 {
			head = contHead
		}
		msgs = append(msgs, toContent(head+cur))
		cur = ""
	}
	for _, b := range blocks {
		if cur != "" && !fits(cur+b) {
			flush()
		}
		cur += b
	}
	if cur != "" {
		flush()
	}
	return msgs
}
