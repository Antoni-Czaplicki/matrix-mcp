package main

import (
	"context"
	"testing"
)

func TestFTSQuery(t *testing.T) {
	cases := map[string]string{
		"hello":           `"hello"*`,
		"hello world":     `"hello" "world"*`,
		`say "hi" OR x`:   `"say" """hi""" "OR" "x"*`,
		"  spaced   out ": `"spaced" "out"*`,
	}
	for in, want := range cases {
		if got := ftsQuery(in); got != want {
			t.Errorf("ftsQuery(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestStoreSearch(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	msgs := []*Message{
		{EventID: "$1", RoomID: "!a:x", Sender: "@alice:x", SenderName: "Alice", TS: 1000, Kind: "text", Body: "Zażółć gęślą jaźń, see https://example.com"},
		{EventID: "$2", RoomID: "!a:x", Sender: "@me:x", SenderName: "Me", TS: 2000, Kind: "text", Body: "thanks for the link"},
		{EventID: "$3", RoomID: "!b:x", Sender: "@bob:x", SenderName: "Bob Builder", TS: 3000, Kind: "file", Body: "", Media: &Media{Filename: "invoice-2026.pdf"}},
	}
	for _, m := range msgs {
		if err := s.Index(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	// Indexing twice is a no-op.
	if err := s.Index(ctx, msgs[0]); err != nil {
		t.Fatal(err)
	}

	search := func(q SearchQuery) []string {
		t.Helper()
		if q.Limit == 0 {
			q.Limit = 10
		}
		hits, err := s.Search(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, h := range hits {
			ids = append(ids, h.EventID)
		}
		return ids
	}
	eq := func(name string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: got %v, want %v", name, got, want)
				return
			}
		}
	}

	eq("diacritics are folded", search(SearchQuery{Text: "zazolc"}), "$1")
	eq("prefix on last word", search(SearchQuery{Text: "exam"}), "$1")
	eq("filename indexed", search(SearchQuery{Text: "invoice"}), "$3")
	eq("room filter", search(SearchQuery{Text: "link", RoomID: "!b:x"}))
	eq("sender by display name", search(SearchQuery{Sender: "builder"}), "$3")
	eq("sender by mxid without query, newest first", search(SearchQuery{Sender: "@me:x"}), "$2")
	eq("date range", search(SearchQuery{Sender: "", Text: "", After: 1500, Before: 3000, RoomID: ""}), "$2")
	eq("limit", search(SearchQuery{After: 1, Limit: 2}), "$3", "$2")

	if err := s.UpdateBody(ctx, "$2", "edited: totally different"); err != nil {
		t.Fatal(err)
	}
	eq("edit replaces old text", search(SearchQuery{Text: "thanks"}))
	eq("edit indexes new text", search(SearchQuery{Text: "totally"}), "$2")

	if err := s.Delete(ctx, "$1"); err != nil {
		t.Fatal(err)
	}
	eq("deleted message gone", search(SearchQuery{Text: "example"}))
	if n, _ := s.IndexedCount(ctx); n != 2 {
		t.Errorf("IndexedCount = %d, want 2", n)
	}
}

func TestActivityRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.SaveActivity(ctx, "!a:x", &activity{TS: 5, Preview: "hi", Sender: "me", Unread: 2, Highlights: 1})
	s.SaveActivity(ctx, "!a:x", &activity{TS: 6, Preview: "yo", Sender: "Bob", Unread: 3})
	got, err := s.LoadActivity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := got["!a:x"]
	if a == nil || a.TS != 6 || a.Preview != "yo" || a.Sender != "Bob" || a.Unread != 3 || a.Highlights != 0 {
		t.Errorf("unexpected activity %+v", a)
	}
}

func TestRebuildFTS(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	// Simulate a v2 row indexed without folding.
	_, _ = s.db.Exec(ctx, `INSERT INTO mcp_messages (event_id, room_id, sender, sender_name, ts, kind, body) VALUES ('$x', '!a:x', '@a:x', 'Łukasz', 1, 'text', 'Łódź')`)
	_, _ = s.db.Exec(ctx, `INSERT INTO mcp_messages_fts (body, sender_name, event_id) VALUES ('Łódź', 'Łukasz', '$x')`)
	if hits, _ := s.Search(ctx, SearchQuery{Text: "lodz", Limit: 5}); len(hits) != 0 {
		t.Fatal("precondition: unfolded row should not match")
	}
	if err := s.rebuildFTS(ctx); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search(ctx, SearchQuery{Text: "lodz", Limit: 5}); len(hits) != 1 {
		t.Errorf("after rebuild: %d hits, want 1", len(hits))
	}
	if hits, _ := s.Search(ctx, SearchQuery{Text: "Łódź", Limit: 5}); len(hits) != 1 {
		t.Errorf("query with diacritics: %d hits, want 1", len(hits))
	}
}
