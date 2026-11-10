package main

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
)

const maxReleaseBody = 15000

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	// GitHub renders single newlines in release notes as line breaks.
	// Raw HTML is passed through; Matrix clients sanitize formatted_body.
	goldmark.WithRendererOptions(gmhtml.WithHardWraps(), gmhtml.WithUnsafe()),
)

var (
	alertRe    = regexp.MustCompile(`(?m)^(\s*>\s*)\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*$`)
	issueRefRe = regexp.MustCompile(`(^|[\s(\[,])#(\d+)\b`)
	mrRefRe    = regexp.MustCompile(`(^|[\s(\[,])!(\d+)\b`)
	shaRefRe   = regexp.MustCompile(`(^|[\s(\[,])([0-9a-f]{7,40})\b`)
	digitRe    = regexp.MustCompile(`[0-9]`)
	letterRe   = regexp.MustCompile(`[a-f]`)
)

var alertTitles = map[string]string{
	"NOTE":      "ℹ️ Note",
	"TIP":       "💡 Tip",
	"IMPORTANT": "❗ Important",
	"WARNING":   "⚠️ Warning",
	"CAUTION":   "🛑 Caution",
}

// linkifyRefs turns "#123" into issue links (and "!123" into merge request
// links on GitLab) and bare commit SHAs into commit links, like the forges
// do, skipping fenced code blocks and inline code.
func linkifyRefs(text string, f *Forge, repo string, markdown bool) string {
	mkLink := func(label, href string) string {
		if markdown {
			return "[" + label + "](" + href + ")"
		}
		return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(label) + `</a>`
	}
	replace := func(s string) string {
		s = issueRefRe.ReplaceAllStringFunc(s, func(m string) string {
			sm := issueRefRe.FindStringSubmatch(m)
			return sm[1] + mkLink("#"+sm[2], f.IssueURL(repo, sm[2]))
		})
		if f.Kind == KindGitLab {
			s = mrRefRe.ReplaceAllStringFunc(s, func(m string) string {
				sm := mrRefRe.FindStringSubmatch(m)
				return sm[1] + mkLink("!"+sm[2], f.MergeRequestURL(repo, sm[2]))
			})
		}
		return shaRefRe.ReplaceAllStringFunc(s, func(m string) string {
			sm := shaRefRe.FindStringSubmatch(m)
			sha := sm[2]
			// Require both a digit and a letter so plain numbers and words aren't linked.
			if !digitRe.MatchString(sha) || !letterRe.MatchString(sha) {
				return m
			}
			return sm[1] + mkLink(sha[:7], f.CommitURL(repo, sha))
		})
	}
	lines := strings.Split(text, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		parts := strings.Split(line, "`")
		for j := 0; j < len(parts); j += 2 {
			parts[j] = replace(parts[j])
		}
		lines[i] = strings.Join(parts, "`")
	}
	return strings.Join(lines, "\n")
}

func renderMarkdown(text string) string {
	var buf strings.Builder
	if err := md.Convert([]byte(text), &buf); err != nil {
		return "<pre>" + html.EscapeString(text) + "</pre>"
	}
	return buf.String()
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func a(href, label string) string {
	return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(label) + `</a>`
}

func toContent(htmlBody string) *event.MessageEventContent {
	content := format.HTMLToContent(htmlBody)
	content.MsgType = event.MsgNotice
	return &content
}

func renderRelease(l Link, r *Release) *event.MessageEventContent {
	var sb strings.Builder
	title := r.Name
	if title == "" {
		title = r.TagName
	}
	fmt.Fprintf(&sb, "<h3>📦 %s: %s</h3>\n", a(l.RepoURL(), l.Repo), a(r.HTMLURL, title))

	var meta []string
	meta = append(meta, "🏷️ <code>"+html.EscapeString(r.TagName)+"</code>")
	if r.Draft {
		meta = append(meta, "<b>Draft</b>")
	}
	if r.Prerelease {
		meta = append(meta, "<b>Pre-release</b>")
	}
	if r.Author.Login != "" {
		meta = append(meta, "by "+a(r.Author.HTMLURL, r.Author.Login))
	}
	if ts := fmtTime(r.PublishedAt); ts != "" {
		meta = append(meta, ts)
	}
	sb.WriteString("<p>" + strings.Join(meta, " · ") + "</p>\n")

	body := strings.TrimSpace(strings.ReplaceAll(r.Body, "\r\n", "\n"))
	if body != "" {
		truncated := false
		if len(body) > maxReleaseBody {
			cut := strings.LastIndex(body[:maxReleaseBody], "\n")
			if cut < maxReleaseBody/2 {
				cut = maxReleaseBody
			}
			body, truncated = body[:cut], true
		}
		body = alertRe.ReplaceAllStringFunc(body, func(m string) string {
			sm := alertRe.FindStringSubmatch(m)
			return sm[1] + "**" + alertTitles[sm[2]] + "**"
		})
		body = linkifyRefs(body, l.Forge, l.Repo, true)
		sb.WriteString(renderMarkdown(body))
		if truncated {
			sb.WriteString("<p><i>… truncated, see " + a(r.HTMLURL, "the release") + " for the rest.</i></p>\n")
		}
	}

	if n := len(r.Assets) + len(r.Sources); n > 0 {
		fmt.Fprintf(&sb, "<p><b>Assets (%d)</b></p>\n<ul>\n", n)
		for _, as := range r.Assets {
			line := a(as.URL, as.Name)
			// External links (Forgejo, GitLab) have no size.
			if as.Size > 0 {
				line += " — " + humanSize(as.Size)
			}
			if as.Digest != "" {
				line += " — <code>" + html.EscapeString(as.Digest) + "</code>"
			}
			sb.WriteString("<li>" + line + "</li>\n")
		}
		for _, as := range r.Sources {
			sb.WriteString("<li>" + a(as.URL, as.Name) + "</li>\n")
		}
		sb.WriteString("</ul>")
	}
	return toContent(sb.String())
}

const maxFiles = 15

func renderCommit(l Link, c *Commit, branch string) *event.MessageEventContent {
	var sb strings.Builder
	short := c.SHA
	if len(short) > 7 {
		short = short[:7]
	}
	header := "🔨 " + a(l.RepoURL(), l.Repo) + "@" + a(c.HTMLURL, short)
	if branch != "" {
		header += " (" + a(l.Forge.TreeURL(l.Repo, branch), branch) + ")"
	}
	sb.WriteString("<h3>" + header + "</h3>\n")

	msg := strings.TrimRight(strings.ReplaceAll(c.Commit.Message, "\r\n", "\n"), "\n")
	subject, rest, _ := strings.Cut(msg, "\n")
	sb.WriteString("<p><b>" + linkifyRefs(html.EscapeString(subject), l.Forge, l.Repo, false) + "</b></p>\n")
	if rest = strings.Trim(rest, "\n"); rest != "" {
		escaped := linkifyRefs(html.EscapeString(rest), l.Forge, l.Repo, false)
		sb.WriteString("<p>" + strings.ReplaceAll(escaped, "\n", "<br>") + "</p>\n")
	}

	var meta []string
	if c.Author != nil && c.Author.Login != "" {
		meta = append(meta, "by "+a(c.Author.HTMLURL, c.Author.Login))
	} else if c.Commit.Author.Name != "" {
		meta = append(meta, "by "+html.EscapeString(c.Commit.Author.Name))
	}
	if ts := fmtTime(c.Commit.Author.Date); ts != "" {
		meta = append(meta, ts)
	}
	if n := len(c.Parents); n > 0 {
		var ps []string
		for _, p := range c.Parents {
			ps = append(ps, a(p.HTMLURL, p.SHA[:min(7, len(p.SHA))]))
		}
		word := "parent"
		if n > 1 {
			word = "parents"
		}
		meta = append(meta, fmt.Sprintf("%d %s %s", n, word, strings.Join(ps, ", ")))
	}
	nFiles, exact := c.NumFiles()
	fileWord := "files"
	if nFiles == 1 && exact {
		fileWord = "file"
	}
	plus := ""
	if !exact {
		plus = "+"
	}
	meta = append(meta, fmt.Sprintf("%d%s %s changed (<font color=\"#2da44e\">+%d</font> <font color=\"#cf222e\">−%d</font>)",
		nFiles, plus, fileWord, c.Stats.Additions, c.Stats.Deletions))
	sb.WriteString("<p>" + strings.Join(meta, " · ") + "</p>\n")

	if len(c.Files) > 0 {
		sb.WriteString("<ul>\n")
		for i, f := range c.Files {
			if i == maxFiles {
				break
			}
			if c.NoFileStats {
				fmt.Fprintf(&sb, "<li><code>%s</code> (%s)</li>\n", html.EscapeString(f.Filename), f.Status)
			} else {
				fmt.Fprintf(&sb, "<li><code>%s</code> (%s, +%d −%d)</li>\n", html.EscapeString(f.Filename), f.Status, f.Additions, f.Deletions)
			}
		}
		if shown := min(len(c.Files), maxFiles); !exact {
			sb.WriteString("<li>… and more</li>\n")
		} else if nFiles > shown {
			fmt.Fprintf(&sb, "<li>… and %d more</li>\n", nFiles-shown)
		}
		sb.WriteString("</ul>")
	}
	return toContent(sb.String())
}
