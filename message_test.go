package main

import "testing"

func TestStripReplyFallback(t *testing.T) {
	cases := map[string]string{
		"plain":                              "plain",
		"> <@a:x> original\n> more\n\nreply": "reply",
		"> <@a:x> original\nreply no blank":  "reply no blank",
		"not > a quote":                      "not > a quote",
	}
	for in, want := range cases {
		if got := stripReplyFallback(in); got != want {
			t.Errorf("stripReplyFallback(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAggregate(t *testing.T) {
	msgs := []*Message{
		{EventID: "$1", Kind: "text", Body: "helo", SenderName: "Alice"},
		{EventID: "$2", Kind: "edit", Edits: "$1", Body: "hello"},
		{EventID: "$3", Kind: "reaction", ReactTo: "$1", Body: "👍", SenderName: "Bob"},
		{EventID: "$4", Kind: "reaction", ReactTo: "$1", Body: "👍", IsMe: true},
		{EventID: "$5", Kind: "image", Media: &Media{Filename: "a.jpg"}},
		{EventID: "$6", Kind: "redaction", Redacts: "$5"},
		{EventID: "$7", Kind: "membership", Body: "@c:x join"},
		{EventID: "$8", Kind: "reaction", ReactTo: "$older", Body: "❤️"},
	}
	out := aggregate(msgs, false)
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(out), out)
	}
	if out[0].Body != "hello" || !out[0].Edited {
		t.Errorf("edit not applied: %+v", out[0])
	}
	if r := out[0].Reactions["👍"]; len(r) != 2 || r[0] != "Bob" || r[1] != "me" {
		t.Errorf("reactions = %v", out[0].Reactions)
	}
	if out[1].Kind != "deleted" || out[1].Media != nil {
		t.Errorf("redaction not applied: %+v", out[1])
	}
	if out[2].EventID != "$8" {
		t.Errorf("reaction to a message outside the page should be kept, got %+v", out[2])
	}
	if n := len(aggregate(msgs, true)); n != 4 {
		t.Errorf("includeState: got %d messages, want 4", n)
	}
}
