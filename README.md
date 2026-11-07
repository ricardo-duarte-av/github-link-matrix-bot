# github-link-matrix-bot

A Matrix bot (mautrix-go) that watches rooms for messages linking to GitHub
commits or releases, fetches the details from the GitHub API, and replies with them.

- Releases: name, tag, pre-release/draft, author, date, rendered release notes,
  assets (size + sha256 digest) and source archives.
- Commits: full message, author, date, parents, branch (taken from RSS-bot
  "Recent Commits to repo:branch:" messages), files changed with +/- stats.

Only new messages are handled; history present at startup is ignored.
Both `m.text` and `m.notice` messages are processed, and replies are sent as
`m.notice` so other bots won't loop on them. E2EE rooms aren't supported.

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
  (uses `$GITHUB_TOKEN` if set)

Set `github_token` in the config (or `$GITHUB_TOKEN`) to avoid GitHub's 60 requests/hour
unauthenticated rate limit.

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

`./data` is mounted read-only at `/data`. The container runs as uid 65532, so
the files must be readable by it (or set `user:` in `docker-compose.yaml`).
