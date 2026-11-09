package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Matches the branch in RSS bot messages like "Recent Commits to repo:main: ...".
var branchRe = regexp.MustCompile(`Recent Commits to [^\s:]+:([^\s:]+):`)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	printURL := flag.String("print", "", "fetch a GitHub commit/release URL, print the message that would be sent, and exit")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.DateTime}).With().Timestamp().Logger()
	if *debug {
		log = log.Level(zerolog.DebugLevel)
	} else {
		log = log.Level(zerolog.InfoLevel)
	}

	if *printURL != "" {
		printPreview(*printURL, os.Getenv("GITHUB_TOKEN"))
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load config")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client, err := mautrix.NewClient(cfg.Homeserver, "", cfg.AccessToken)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create client")
	}
	client.Log = log
	client.SyncPresence = event.PresenceOffline

	whoami, err := client.Whoami(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("Whoami failed, check homeserver and access token")
	}
	client.UserID = whoami.UserID
	client.DeviceID = whoami.DeviceID
	log.Info().Stringer("user_id", whoami.UserID).Stringer("device_id", whoami.DeviceID).Msg("Logged in")

	monitored := map[id.RoomID]bool{}
	for _, room := range cfg.Rooms {
		resp, err := client.JoinRoom(ctx, room, nil)
		if err != nil {
			log.Error().Err(err).Str("room", room).Msg("Failed to join room")
			continue
		}
		monitored[resp.RoomID] = true
		log.Info().Stringer("room_id", resp.RoomID).Str("room", room).Msg("Monitoring room")
		var enc event.EncryptionEventContent
		if err := client.StateEvent(ctx, resp.RoomID, event.StateEncryption, "", &enc); err == nil && enc.Algorithm != "" {
			log.Warn().Stringer("room_id", resp.RoomID).Msg("Room is encrypted; this bot does not support E2EE and won't see messages there")
		}
	}
	if len(monitored) == 0 {
		log.Fatal().Msg("Not in any monitored room")
	}

	rooms := make([]id.RoomID, 0, len(monitored))
	for r := range monitored {
		rooms = append(rooms, r)
	}

	gh := &GitHub{Token: cfg.GitHubToken, Client: &http.Client{Timeout: 20 * time.Second}}
	startTime := time.Now()

	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	syncer.FilterJSON = &mautrix.Filter{
		Room: &mautrix.RoomFilter{
			Rooms:    rooms,
			Timeline: &mautrix.FilterPart{Types: []event.Type{event.EventMessage}},
		},
		Presence:    &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
		AccountData: &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
	}
	// Skip the initial sync entirely so messages already in the room are ignored.
	syncer.OnSync(client.DontProcessOldEvents)
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		if !monitored[evt.RoomID] || evt.Sender == client.UserID {
			return
		}
		// Extra guard against old events (e.g. after a gappy sync).
		if time.UnixMilli(evt.Timestamp).Before(startTime.Add(-time.Minute)) {
			return
		}
		content := evt.Content.AsMessage()
		if content == nil || (content.MsgType != event.MsgText && content.MsgType != event.MsgNotice) {
			return
		}
		if content.RelatesTo.GetReplaceID() != "" {
			return // ignore edits
		}
		handleMessage(ctx, client, gh, evt, content)
	})

	go func() {
		<-ctx.Done()
		client.StopSync()
	}()

	log.Info().Msg("Starting sync")
	for ctx.Err() == nil {
		err := client.SyncWithContext(ctx)
		if err == nil || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			break
		}
		log.Error().Err(err).Msg("Sync failed, retrying in 10s")
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
	log.Info().Msg("Stopped")
}

const (
	// A release or commit can 404 when the RSS feed is ahead of GitHub (e.g. a
	// release that is still a draft). Keep retrying for a while before giving up.
	// Rate-limited fetches are retried within the same window, once the limit resets.
	notFoundRetryInterval = 5 * time.Minute
	retryFor              = 24 * time.Hour
)

func handleMessage(ctx context.Context, client *mautrix.Client, gh *GitHub, evt *event.Event, msg *event.MessageEventContent) {
	links := findLinks(msg.Body, msg.FormattedBody)
	if len(links) == 0 {
		return
	}
	log := client.Log.With().Stringer("room_id", evt.RoomID).Stringer("event_id", evt.ID).Logger()

	branch := ""
	if m := branchRe.FindStringSubmatch(msg.Body); m != nil {
		branch = m[1]
	}

	for _, l := range links {
		content, err := gh.Fetch(ctx, l, branch)
		if wait, ok := retryDelay(err); ok {
			logRetry(log, err, l, 1, wait)
			go retryLater(ctx, client, gh, evt.RoomID, l, branch, wait, log)
			continue
		} else if err != nil {
			log.Error().Err(err).Str("url", l.URL).Msg("Failed to fetch from GitHub")
			continue
		}
		postContent(ctx, client, evt.RoomID, l, content, log)
	}
}

// retryDelay reports whether a failed fetch should be retried and after how long:
// once the rate limit resets, or after notFoundRetryInterval for a 404.
func retryDelay(err error) (time.Duration, bool) {
	if wait, ok := rateLimitWait(err); ok {
		return wait, true
	}
	if isNotFound(err) {
		return notFoundRetryInterval, true
	}
	return 0, false
}

func logRetry(log zerolog.Logger, err error, l Link, attempt int, wait time.Duration) {
	ev := log.Warn().Str("url", l.URL).Int("attempt", attempt).Dur("retry_in", wait)
	if isNotFound(err) {
		ev.Msg("Not found on GitHub yet, will retry")
	} else if _, ok := rateLimitWait(err); ok {
		ev.Msg("GitHub rate limit exceeded, will retry after it resets")
	} else {
		ev.Err(err).Msg("Failed to fetch from GitHub, will retry")
	}
}

func retryLater(ctx context.Context, client *mautrix.Client, gh *GitHub, roomID id.RoomID, l Link, branch string, wait time.Duration, log zerolog.Logger) {
	deadline := time.Now().Add(retryFor)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for attempt := 2; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		content, err := gh.Fetch(ctx, l, branch)
		if err == nil {
			postContent(ctx, client, roomID, l, content, log)
			return
		}
		if time.Now().After(deadline) {
			log.Error().Err(err).Str("url", l.URL).Int("attempt", attempt).Msg("Still failing to fetch from GitHub, giving up")
			return
		}
		// Other transient errors (network, 5xx) don't end the retry loop either.
		next, ok := retryDelay(err)
		if !ok {
			next = notFoundRetryInterval
		}
		logRetry(log, err, l, attempt, next)
		timer.Reset(next)
	}
}

func postContent(ctx context.Context, client *mautrix.Client, roomID id.RoomID, l Link, content *event.MessageEventContent, log zerolog.Logger) {
	// Post as a standalone message (not a reply) that pings nobody.
	content.Mentions = &event.Mentions{}
	if _, err := client.SendMessageEvent(ctx, roomID, event.EventMessage, content); err != nil {
		log.Error().Err(err).Str("url", l.URL).Msg("Failed to send message")
		return
	}
	log.Info().Str("url", l.URL).Msg("Posted GitHub info")
}

func printPreview(u, token string) {
	gh := &GitHub{Token: token, Client: &http.Client{Timeout: 20 * time.Second}}
	links := findLinks(u)
	if len(links) == 0 {
		fmt.Fprintln(os.Stderr, "no GitHub commit/release link found")
		os.Exit(1)
	}
	branch := ""
	if m := branchRe.FindStringSubmatch(u); m != nil {
		branch = m[1]
	}
	for _, l := range links {
		content, err := gh.Fetch(context.Background(), l, branch)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("===== body =====\n%s\n===== formatted_body =====\n%s\n", content.Body, content.FormattedBody)
	}
}
