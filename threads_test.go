package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestThreadRelationJSON(t *testing.T) {
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: "x"}
	content.RelatesTo = threadRelation("$root", "$rss")
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		RelatesTo map[string]any `json:"m.relates_to"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	rel := got.RelatesTo
	if rel["rel_type"] != "m.thread" || rel["event_id"] != "$root" {
		t.Errorf("thread relation wrong: %v", rel)
	}
	if fb, ok := rel["is_falling_back"]; ok && fb != false {
		t.Errorf("is_falling_back = %v, want false or absent", fb)
	}
	if r, _ := rel["m.in_reply_to"].(map[string]any); r["event_id"] != "$rss" {
		t.Errorf("m.in_reply_to wrong: %v", rel["m.in_reply_to"])
	}
	// mautrix must treat it as a real reply, not a thread fallback.
	if content.RelatesTo.GetReplyTo() != "$rss" {
		t.Errorf("GetReplyTo = %q, want $rss", content.RelatesTo.GetReplyTo())
	}
}

func TestThreadKey(t *testing.T) {
	gh := threadKey(Entry{Repo: "Owner/Repo"})
	fj := threadKey(Entry{Repo: "owner/repo", Forge: KindForgejo, RepoURL: "https://codeberg.org/owner/repo"})
	if gh != "https://github.com/owner/repo" {
		t.Errorf("github key = %q", gh)
	}
	if gh == fj {
		t.Errorf("same repo name on different forges shares a key: %q", gh)
	}
}

func TestThreadsStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	cfg := ThreadsConfig{Enabled: true, StateFile: path}
	th, err := NewThreads(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	room := id.RoomID("!room:example.org")
	th.rooms[room] = map[string]id.EventID{"https://github.com/owner/repo": "$root"}
	th.saveLocked()

	th2, err := NewThreads(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if got := th2.rooms[room]["https://github.com/owner/repo"]; got != "$root" {
		t.Errorf("loaded root = %q, want $root", got)
	}
}
