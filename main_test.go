package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestMentionMatch(t *testing.T) {
	for msg, want := range map[string]bool{
		"@alice": true, "hi @alice.": true, "@alice, ping": true, "@alice-": true, "@ALICE:": true, "@alice\nmore": true,
		"@alicebob": false, "@alice.bob": false, "@alice_bob": false, "@alice-bob": false, "alice": false,
	} {
		if got := len(newest([]post{{Message: msg}}, "alice", 5)) == 1; got != want {
			t.Errorf("%q: got %v, want %v", msg, got, want)
		}
	}
	// QuoteMeta: the dot in "a.b" is literal
	if len(newest([]post{{Message: "@axb"}}, "a.b", 5)) != 0 || len(newest([]post{{Message: "@a.b"}}, "a.b", 5)) != 1 {
		t.Error("username with a dot")
	}
}

func TestInOrder(t *testing.T) {
	// Order is newest-first and leaves out the old thread root "r" that Posts carries.
	pl := postList{
		Order: []string{"c", "b", "a", "gone"},
		Posts: map[string]post{"r": {ID: "r", CreateAt: 1}, "a": {ID: "a", CreateAt: 5}, "b": {ID: "b", CreateAt: 5}, "c": {ID: "c", CreateAt: 9}},
	}
	got := inOrder(pl)
	if len(got) != 3 || got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatalf("got %+v", got)
	}
}

func TestChannelsBatchesDMUsers(t *testing.T) {
	lookups := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/users/me/teams/t1/channels":
			_ = json.NewEncoder(w).Encode([]channel{
				{ID: "d1", Name: "me__u1", Type: "D"}, {ID: "d2", Name: "u2__me", Type: "D"},
				{ID: "d3", Name: "me__me", Type: "D"}, {ID: "o1", Name: "town-square", Type: "O"},
			})
		case "/api/v4/users/me/teams/t1/channels/members":
			_, _ = w.Write([]byte("[]"))
		case "/api/v4/users/ids":
			lookups++
			var ids []string
			_ = json.NewDecoder(r.Body).Decode(&ids)
			us := []user{}
			for _, id := range ids {
				us = append(us, user{ID: id, Username: "name-" + id})
			}
			_ = json.NewEncoder(w).Encode(us)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "x", me: user{ID: "me", Username: "me"}, team: team{ID: "t1"},
		users: map[string]string{"me": "me"}, chans: map[string]string{}}

	if err := c.channels(opts{}); err != nil {
		t.Fatal(err)
	}
	if lookups != 1 {
		t.Fatalf("user lookups = %d, want 1", lookups)
	}
	for name, want := range map[string]string{"me__u1": "@name-u1", "u2__me": "@name-u2", "me__me": "@me"} {
		if got := c.label(channel{Name: name, Type: "D"}); got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
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
