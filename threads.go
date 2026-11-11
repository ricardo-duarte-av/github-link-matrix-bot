package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const threadReaction = "🧵"

// maxReacted bounds the in-memory set of rssbot events already reacted to.
const maxReacted = 10000

// Threads posts link info into one thread per (room, repo). The thread root is a
// short message the bot sends the first time it sees a repo in a room.
type Threads struct {
	cfg ThreadsConfig
	log zerolog.Logger

	mu      sync.Mutex
	rooms   map[id.RoomID]map[string]id.EventID // room -> repo URL (lowercase) -> root
	checked map[id.EventID]bool                 // roots confirmed to still exist this run
	reacted map[id.EventID]bool                 // rssbot events already reacted to
}

type threadsState struct {
	Rooms map[id.RoomID]map[string]id.EventID `json:"rooms"`
}

func NewThreads(cfg ThreadsConfig, log zerolog.Logger) (*Threads, error) {
	t := &Threads{
		cfg:     cfg,
		log:     log,
		rooms:   map[id.RoomID]map[string]id.EventID{},
		checked: map[id.EventID]bool{},
		reacted: map[id.EventID]bool{},
	}
	if cfg.StateFile == "" {
		log.Warn().Msg("threads.state_file not set; thread roots are kept in memory only and new threads are started after a restart")
		return t, nil
	}
	data, err := os.ReadFile(cfg.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	} else if err != nil {
		return nil, fmt.Errorf("read threads state: %w", err)
	}
	var st threadsState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse threads state %s: %w", cfg.StateFile, err)
	}
	if st.Rooms != nil {
		t.rooms = st.Rooms
	}
	return t, nil
}

// threadKey identifies a repo across forges, e.g. https://github.com/owner/repo.
func threadKey(e Entry) string {
	_, repoURL := e.forge()
	return strings.ToLower(repoURL)
}

// threadRelation makes the message part of the thread at root and a real reply
// (not a fallback) to replyTo, the rssbot message outside the thread. Clients
// without thread support show it as a plain reply to replyTo.
func threadRelation(root, replyTo id.EventID) *event.RelatesTo {
	rel := &event.RelatesTo{Type: event.RelThread, EventID: root}
	if replyTo != "" {
		rel.InReplyTo = &event.InReplyTo{EventID: replyTo}
	}
	return rel
}

func rootContent(e Entry) *event.MessageEventContent {
	_, repoURL := e.forge()
	return &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    fmt.Sprintf("%s %s — commits & releases (%s)", threadReaction, e.Repo, repoURL),
		Format:  event.FormatHTML,
		FormattedBody: fmt.Sprintf(`%s <a href="%s">%s</a> — commits &amp; releases`,
			threadReaction, html.EscapeString(repoURL), html.EscapeString(e.Repo)),
		Mentions: &event.Mentions{},
	}
}

// root returns the thread root for the entry's repo in the room, creating it if
// there is none yet or the stored one was redacted.
func (t *Threads) root(ctx context.Context, client *mautrix.Client, roomID id.RoomID, e Entry) (id.EventID, error) {
	key := threadKey(e)
	t.mu.Lock()
	defer t.mu.Unlock()
	if root := t.rooms[roomID][key]; root != "" {
		if t.checked[root] {
			return root, nil
		}
		evt, err := client.GetEvent(ctx, roomID, root)
		switch {
		case errors.Is(err, mautrix.MNotFound):
			t.log.Warn().Stringer("room_id", roomID).Str("repo", key).Stringer("root", root).Msg("Thread root not found, starting a new thread")
		case err != nil:
			// Can't tell; keep using it rather than risk duplicate threads.
			t.log.Warn().Err(err).Stringer("root", root).Msg("Failed to check thread root, using it anyway")
			return root, nil
		case evt.Unsigned.RedactedBecause != nil || len(evt.Content.Raw) == 0:
			t.log.Warn().Stringer("room_id", roomID).Str("repo", key).Stringer("root", root).Msg("Thread root was redacted, starting a new thread")
		default:
			t.checked[root] = true
			return root, nil
		}
	}
	resp, err := client.SendMessageEvent(ctx, roomID, event.EventMessage, rootContent(e))
	if err != nil {
		return "", fmt.Errorf("send thread root: %w", err)
	}
	if t.rooms[roomID] == nil {
		t.rooms[roomID] = map[string]id.EventID{}
	}
	t.rooms[roomID][key] = resp.EventID
	t.checked[resp.EventID] = true
	t.saveLocked()
	t.log.Info().Stringer("room_id", roomID).Str("repo", key).Stringer("root", resp.EventID).Msg("Started thread")
	return resp.EventID, nil
}

// forget drops a root the homeserver rejected, unless it was already replaced.
func (t *Threads) forget(roomID id.RoomID, e Entry, root id.EventID) {
	key := threadKey(e)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rooms[roomID][key] == root {
		delete(t.rooms[roomID], key)
		delete(t.checked, root)
		t.saveLocked()
	}
}

// Send posts content into the repo's thread as a reply to replyTo, then reacts
// to replyTo once. If the thread can't be used, it posts to the room instead.
func (t *Threads) Send(ctx context.Context, client *mautrix.Client, roomID id.RoomID, replyTo id.EventID, e Entry, content *event.MessageEventContent) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var root id.EventID
		root, err = t.root(ctx, client, roomID, e)
		if err != nil {
			break
		}
		content.RelatesTo = threadRelation(root, replyTo)
		_, err = client.SendMessageEvent(ctx, roomID, event.EventMessage, content)
		if err == nil {
			t.react(ctx, client, roomID, replyTo)
			return nil
		}
		var httpErr mautrix.HTTPError
		if !errors.As(err, &httpErr) || httpErr.Response == nil || httpErr.Response.StatusCode != http.StatusBadRequest {
			return err
		}
		// e.g. the root can no longer start a thread; start a new one.
		t.log.Warn().Err(err).Stringer("root", root).Msg("Homeserver rejected thread message, starting a new thread")
		t.forget(roomID, e, root)
	}
	t.log.Warn().Err(err).Msg("Could not post in thread, posting to the room instead")
	content.RelatesTo = nil
	_, err = client.SendMessageEvent(ctx, roomID, event.EventMessage, content)
	return err
}

func (t *Threads) react(ctx context.Context, client *mautrix.Client, roomID id.RoomID, eventID id.EventID) {
	if eventID == "" {
		return
	}
	t.mu.Lock()
	if t.reacted[eventID] {
		t.mu.Unlock()
		return
	}
	if len(t.reacted) >= maxReacted {
		t.reacted = map[id.EventID]bool{}
	}
	t.reacted[eventID] = true
	t.mu.Unlock()
	if _, err := client.SendReaction(ctx, roomID, eventID, threadReaction); err != nil {
		t.log.Warn().Err(err).Stringer("event_id", eventID).Msg("Failed to react to message")
	}
}

func (t *Threads) saveLocked() {
	if t.cfg.StateFile == "" {
		return
	}
	data, err := json.Marshal(threadsState{Rooms: t.rooms})
	if err != nil {
		t.log.Error().Err(err).Msg("Failed to encode threads state")
		return
	}
	tmp := t.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.log.Error().Err(err).Str("path", tmp).Msg("Failed to write threads state")
		return
	}
	if err := os.Rename(tmp, t.cfg.StateFile); err != nil {
		t.log.Error().Err(err).Str("path", t.cfg.StateFile).Msg("Failed to write threads state")
	}
}
