package main

import (
	"testing"
	"time"
)

func TestParseArgs(t *testing.T) {
	o, err := parseArgs([]string{"general", "-n", "5", "hello", "world", "--since", "3d", "-r", "abc", "-f", "a.txt", "-f", "b.txt", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.pos) != 3 || o.pos[0] != "general" || o.n != 5 || o.since != 72*time.Hour ||
		o.reply != "abc" || len(o.files) != 2 || !o.json {
		t.Fatalf("got %+v", o)
	}
	for _, bad := range [][]string{{"-n"}, {"-n", "x"}, {"--since", "2w"}, {"--since", "-1h"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}
	if d, _ := parseSince("90m"); d != 90*time.Minute {
		t.Errorf("90m = %v", d)
	}
}

func TestNewest(t *testing.T) {
	posts := []post{{ID: "1", Message: "hi @Alice"}, {ID: "2", Message: "alice is here"}, {ID: "3", Message: "@alice ping"}, {ID: "4", Message: "x"}}
	if got := newest(append([]post(nil), posts...), "alice", 5); len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" {
		t.Fatalf("mention filter: %+v", got)
	}
	if got := newest(append([]post(nil), posts...), "", 2); len(got) != 2 || got[0].ID != "3" || got[1].ID != "4" {
		t.Fatalf("limit keeps newest: %+v", got)
	}
}

func TestAttachmentText(t *testing.T) {
	props := map[string]any{"attachments": []any{map[string]any{"title": "CI failed", "text": "build #12"}}}
	if got := attachmentText(props); got != "\n▸ CI failed\n▸ build #12" {
		t.Fatalf("got %q", got)
	}
	if attachmentText(nil) != "" {
		t.Fatal("nil props should give empty text")
	}
}
