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
		fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var content *event.MessageEventContent
		var err error
		switch l.Kind {
		case "release":
			var r *Release
			if r, err = gh.Release(fetchCtx, l.Owner, l.Repo, l.Ref); err == nil {
				content = renderRelease(l, r)
			}
		case "commit":
			var c *Commit
			if c, err = gh.Commit(fetchCtx, l.Owner, l.Repo, l.Ref); err == nil {
				content = renderCommit(l, c, branch)
			}
		}
		cancel()
		if err != nil {
			log.Error().Err(err).Str("url", l.URL).Msg("Failed to fetch from GitHub")
			continue
		}
		content.SetReply(evt)
		// Don't ping the original sender.
		content.Mentions = &event.Mentions{}
		if _, err := client.SendMessageEvent(ctx, evt.RoomID, event.EventMessage, content); err != nil {
			log.Error().Err(err).Str("url", l.URL).Msg("Failed to send message")
			continue
		}
		log.Info().Str("url", l.URL).Msg("Posted GitHub info")
	}
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
		var content *event.MessageEventContent
		var err error
		if l.Kind == "release" {
			var r *Release
			if r, err = gh.Release(context.Background(), l.Owner, l.Repo, l.Ref); err == nil {
				content = renderRelease(l, r)
			}
		} else {
			var c *Commit
			if c, err = gh.Commit(context.Background(), l.Owner, l.Repo, l.Ref); err == nil {
				content = renderCommit(l, c, branch)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("===== body =====\n%s\n===== formatted_body =====\n%s\n", content.Body, content.FormattedBody)
	}
}
