package main

import (
	"testing"

	"maunium.net/go/mautrix/id"
)

func testRoomCache() *RoomCache {
	return &RoomCache{rooms: map[id.RoomID]*RoomInfo{}, invites: map[id.RoomID]bool{}, loading: map[id.RoomID]bool{}}
}

func TestNoteJoinedClearsInviteAndRequestsLoad(t *testing.T) {
	rc := testRoomCache()
	room := id.RoomID("!new:example.com")
	rc.AddInvite(room)
	if got := rc.Invites(); len(got) != 1 {
		t.Fatalf("invites = %v, want the room", got)
	}
	if !rc.NoteJoined(room, true) {
		t.Fatal("a newly joined room should be loaded")
	}
	if got := rc.Invites(); len(got) != 0 {
		t.Fatalf("invites = %v, want none after joining", got)
	}
	if rc.NoteJoined(room, true) {
		t.Fatal("a room already loading should not be loaded twice")
	}
	rc.FinishLoading(room)
	if !rc.NoteJoined(room, true) {
		t.Fatal("a failed load should be retried on the next sync")
	}
}

func TestNoteJoinedKnownRoomAndStartup(t *testing.T) {
	rc := testRoomCache()
	known, starting := id.RoomID("!known:example.com"), id.RoomID("!starting:example.com")
	rc.rooms[known] = &RoomInfo{ID: known}
	if rc.NoteJoined(known, true) {
		t.Fatal("a known room should not be reloaded")
	}
	rc.AddInvite(starting)
	if rc.NoteJoined(starting, false) {
		t.Fatal("startup leaves loading to LoadAll")
	}
	if got := rc.Invites(); len(got) != 0 {
		t.Fatalf("invites = %v, want the invite cleared during startup too", got)
	}
}

func TestClassifyRoom(t *testing.T) {
	cases := []struct {
		name   string
		others int
		hasBot bool
		ghosts int
		portal bool
		want   string
	}{
		{"management room", 1, true, 0, false, "bridge_bot"},
		{"bridged channel with only the bot", 1, true, 0, true, "group"},
		{"bridged dm", 2, true, 1, true, "dm"},
		{"native dm", 1, false, 0, false, "dm"},
		{"group", 5, true, 4, true, "group"},
	}
	for _, c := range cases {
		if got := classifyRoom(c.others, c.hasBot, c.ghosts, c.portal); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
