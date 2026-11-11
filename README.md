# github-link-matrix-bot

A Matrix bot (mautrix-go) that watches rooms for messages linking to commits or
releases on GitHub, Forgejo/Gitea or GitLab, fetches the details from the forge's
API, and posts them.

github.com, gitlab.com and codeberg.org work out of the box; self-hosted
instances (e.g. `https://git.eden-emu.dev`) are added under `forges:` in the
config. Forgejo and GitLab don't report per-file line counts, so changed files
there are listed with their status only.

- Releases: name, tag, pre-release/draft, author, date, rendered release notes,
  assets (size + sha256 digest) and source archives.
- Commits: full message, author, date, parents, branch (taken from RSS-bot
  "Recent Commits to repo:branch:" messages), files changed with +/- stats.

- Daily summary (optional): once a day, each room gets a digest of the commits
  and releases posted there, grouped by repo, with links. See `summary:` in
  `config.yaml.example`. The log is persisted to `state_file`, so restarts
  don't lose it; a missed summary (bot down at that time) is folded into the
  next one. Digests over ~48 KB are split into several messages (Matrix
  events are capped at 64 KiB).

- Threads (optional): instead of posting into the main timeline, the info goes
  into one thread per repo per room, as a reply to the message with the link,
  and that message gets a 🧵 reaction. The bot starts each thread with a short
  "🧵 owner/repo — commits & releases" message the first time it sees the repo.
  Thread roots are kept in `state_file`, so restarts reuse the same threads; if a
  root is redacted, a new thread is started. See `threads:` in
  `config.yaml.example`.

Only new messages are handled; history present at startup is ignored.
Both `m.text` and `m.notice` messages are processed, and the info is posted as a
standalone `m.notice` (or a thread reply, with threads on) that mentions nobody,
and the bot ignores its own messages. E2EE rooms
aren't supported.

## Setup

```sh
cp config.yaml.example config.yaml   # edit it; config.yaml and .token are git-ignored
go build -o github-link-matrix-bot .
./github-link-matrix-bot -config config.yaml
```

Flags:

- `-config path` – config file (default `config.yaml`)
- `-debug` – verbose logging
- `-print URL` – fetch a commit/release URL, print the message it would send, and exit
  (uses the forges from `-config` if it loads, else the built-in ones and `$GITHUB_TOKEN`)

Set `github_token` in the config (or `$GITHUB_TOKEN`) to avoid GitHub's 60 requests/hour
unauthenticated rate limit; other forges take an optional `token` in their `forges:`
entry. If a request is rate limited anyway (GitHub's headers, or a 429 with
`Retry-After` from GitLab), the link is retried once the limit resets (within the same
24h window used for links that 404).

## Docker

Every push builds a multi-arch (amd64/arm64) image and publishes it to
`ghcr.io/ricardo-duarte-av/github-link-matrix-bot`, tagged with the branch
name, `sha-<commit>`, and `latest` for the default branch.

```sh
mkdir data
cp config.yaml.example data/config.yaml   # edit; keep access_token_file: .token
cp .token data/.token
docker compose up -d
```

`./data` is mounted read-only at `/data`; the daily summary log and thread
roots go in the writable `state` named volume at `/state`. The container runs as uid 65532, so
the files must be readable by it (or set `user:` in `docker-compose.yaml`).
