// mmdms is a small Mattermost CLI for one user: read, post, search, react.
// It talks to the REST API v4 with a session token it gets by logging in with
// the password from the macOS Keychain, and logs in again when the session expires.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const usage = `mmdms: Mattermost from the terminal

  mmdms channels [--unread]                     my channels with unread / mention counts
  mmdms read <channel|@user> [-n 30] [--since 2h]
  mmdms thread <post-id>
  mmdms post <channel|@user> <text|-> [--reply <post-id>] [--file <path>]...
  mmdms search <terms...> [-n 20]               Mattermost syntax: from:x in:y after:2026-10-01 "exact"
  mmdms mentions [-n 20]                        posts that mention me
  mmdms react <post-id> <emoji>
  mmdms whoami

  --json   JSON output (channels, read, thread, search, mentions)
  text "-" reads the message from stdin.

Env (required): MM_URL (server URL), MM_USER (username).
Password: Keychain item service=<MM_URL host>, account=<MM_USER>.
Session token cache: ~/.config/mmdms/<host>_<user>.token`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mmdms:", err)
		os.Exit(1)
	}
}

// ---- args ----

type opts struct {
	pos    []string
	n      int
	since  time.Duration
	reply  string
	files  []string
	json   bool
	unread bool
}

// parseArgs allows flags anywhere, so "read town-square -n 5" works.
func parseArgs(args []string) (opts, error) {
	o := opts{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		var v string
		switch a {
		case "--json":
			o.json = true
		case "--unread":
			o.unread = true
		case "-n", "--number":
			if v, err = val(); err == nil {
				o.n, err = strconv.Atoi(v)
			}
		case "--since":
			if v, err = val(); err == nil {
				o.since, err = parseSince(v)
			}
		case "-r", "--reply":
			o.reply, err = val()
		case "-f", "--file":
			if v, err = val(); err == nil {
				o.files = append(o.files, v)
			}
		default:
			o.pos = append(o.pos, a)
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

// parseSince accepts Go durations plus a "d" (days) suffix: 30m, 2h, 3d.
func parseSince(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad --since %q (use 30m, 2h, 3d)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad --since %q (use 30m, 2h, 3d)", s)
	}
	return d, nil
}

// ---- client ----

type client struct {
	base, host, user, token string
	me                      user
	team                    team
	users                   map[string]string // id -> username
	chans                   map[string]string // id -> display label
}

type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type channel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DisplayName   string `json:"display_name"`
	Type          string `json:"type"`
	TotalMsgCount int64  `json:"total_msg_count"`
}

type post struct {
	ID        string         `json:"id"`
	CreateAt  int64          `json:"create_at"`
	UserID    string         `json:"user_id"`
	ChannelID string         `json:"channel_id"`
	RootID    string         `json:"root_id"`
	Message   string         `json:"message"`
	Type      string         `json:"type"`
	FileIDs   []string       `json:"file_ids"`
	Props     map[string]any `json:"props"`
}

type postList struct {
	Order []string        `json:"order"`
	Posts map[string]post `json:"posts"`
}

type rawBody struct {
	ct   string
	data []byte
}

var errUnauthorized = errors.New("unauthorized")

// httpc caps every request, uploads included, so a stalled server can't hang the CLI.
var httpc = &http.Client{Timeout: 60 * time.Second}

// tokenPath is the session token cache, one file per server and user.
func (c *client) tokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "mmdms", c.host+"_"+c.user+".token")
}

func newClient() (*client, error) {
	base, usr := strings.TrimRight(os.Getenv("MM_URL"), "/"), os.Getenv("MM_USER")
	if base == "" || usr == "" {
		return nil, errors.New("set MM_URL (e.g. https://chat.example.com) and MM_USER")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad MM_URL %q", base)
	}
	c := &client{base: base, host: u.Host, user: usr, users: map[string]string{}, chans: map[string]string{}}
	if b, err := os.ReadFile(c.tokenPath()); err == nil {
		c.token = strings.TrimSpace(string(b))
	}
	return c, nil
}

func (c *client) login() error {
	pw, err := exec.Command("security", "find-generic-password", "-s", c.host, "-a", c.user, "-w").Output()
	if err != nil {
		return fmt.Errorf("no Keychain password for service=%s account=%s", c.host, c.user)
	}
	body, _ := json.Marshal(map[string]string{"login_id": c.user, "password": strings.TrimRight(string(pw), "\n")})
	resp, err := httpc.Post(c.base+"/api/v4/users/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login as %s failed: %s", c.user, apiError(resp))
	}
	c.token = resp.Header.Get("Token")
	if err := os.MkdirAll(filepath.Dir(c.tokenPath()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.tokenPath(), []byte(c.token), 0o600)
}

func apiError(resp *http.Response) string {
	var e struct {
		Message string `json:"message"`
	}
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return fmt.Sprintf("%d %s", resp.StatusCode, e.Message)
	}
	return resp.Status
}

// call does one API request (JSON body unless it's a rawBody), logging in
// first when there's no token and once more on 401.
func (c *client) call(method, path string, body, out any) error {
	ct, payload := "application/json", []byte(nil)
	switch b := body.(type) {
	case nil:
	case rawBody:
		ct, payload = b.ct, b.data
	default:
		payload, _ = json.Marshal(b)
	}
	for range 2 {
		if c.token == "" {
			if err := c.login(); err != nil {
				return err
			}
		}
		err := c.once(method, path, ct, payload, out)
		if !errors.Is(err, errUnauthorized) {
			return err
		}
		c.token = ""
	}
	return fmt.Errorf("%s %s: unauthorized after re-login", method, path)
}

func (c *client) once(method, path, ct string, payload []byte, out any) error {
	var r io.Reader
	if payload != nil {
		r = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.base+"/api/v4"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if payload != nil {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, path, apiError(resp))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *client) init() error {
	if err := c.call("GET", "/users/me", nil, &c.me); err != nil {
		return err
	}
	c.users[c.me.ID] = c.me.Username
	var teams []team
	if err := c.call("GET", "/users/me/teams", nil, &teams); err != nil {
		return err
	}
	if len(teams) == 0 {
		return errors.New("user is in no team")
	}
	c.team = teams[0] // ponytail: single-team server; add --team if a second team appears
	return nil
}

// resolve turns "town-square", "~town-square", "@user" or a channel ID into a channel ID.
func (c *client) resolve(target string) (string, error) {
	if name, ok := strings.CutPrefix(target, "@"); ok {
		var u user
		if err := c.call("GET", "/users/username/"+url.PathEscape(name), nil, &u); err != nil {
			return "", err
		}
		var ch channel
		if err := c.call("POST", "/channels/direct", []string{c.me.ID, u.ID}, &ch); err != nil {
			return "", err
		}
		return ch.ID, nil
	}
	name := strings.TrimPrefix(target, "~")
	var ch channel
	err := c.call("GET", "/teams/"+c.team.ID+"/channels/name/"+url.PathEscape(name), nil, &ch)
	if err != nil && len(name) == 26 {
		err = c.call("GET", "/channels/"+name, nil, &ch)
	}
	if err != nil {
		return "", fmt.Errorf("channel %q: %w", target, err)
	}
	return ch.ID, nil
}

func (c *client) loadUsers(ids []string) {
	var missing []string
	for _, id := range ids {
		if _, ok := c.users[id]; !ok && id != "" {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return
	}
	var us []user
	if c.call("POST", "/users/ids", missing, &us) == nil {
		for _, u := range us {
			c.users[u.ID] = u.Username
		}
	}
}

// label is how a channel is shown: ~name, @other-user for DMs, display name for group DMs.
func (c *client) label(ch channel) string {
	switch ch.Type {
	case "D":
		other := c.dmOther(ch)
		c.loadUsers([]string{other})
		return "@" + c.users[other]
	case "G":
		return ch.DisplayName
	}
	return "~" + ch.Name
}

// dmOther is the other user of a DM channel named "<id>__<id>"; a DM with yourself is "me__me".
func (c *client) dmOther(ch channel) string {
	other := c.me.ID
	for _, id := range strings.Split(ch.Name, "__") {
		if id != c.me.ID {
			other = id
		}
	}
	return other
}

func (c *client) channelLabel(id string) string {
	if l, ok := c.chans[id]; ok {
		return l
	}
	var ch channel
	l := id
	if c.call("GET", "/channels/"+id, nil, &ch) == nil {
		l = c.label(ch)
	}
	c.chans[id] = l
	return l
}

// ---- output ----

type outPost struct {
	ID      string `json:"id"`
	RootID  string `json:"root_id,omitempty"`
	Channel string `json:"channel,omitempty"`
	User    string `json:"user"`
	Time    string `json:"time"`
	Message string `json:"message"`
	Files   int    `json:"files,omitempty"`
}

func (c *client) render(posts []post, withChannel, asJSON bool) error {
	ids := make([]string, 0, len(posts))
	for _, p := range posts {
		ids = append(ids, p.UserID)
	}
	c.loadUsers(ids)
	out := make([]outPost, 0, len(posts))
	for _, p := range posts {
		if strings.HasPrefix(p.Type, "system_") {
			continue
		}
		o := outPost{
			ID: p.ID, RootID: p.RootID, User: c.users[p.UserID],
			Time:    time.UnixMilli(p.CreateAt).Format(time.RFC3339),
			Message: p.Message + attachmentText(p.Props), Files: len(p.FileIDs),
		}
		if name, _ := p.Props["override_username"].(string); name != "" {
			o.User = name + " (webhook)"
		}
		if withChannel {
			o.Channel = c.channelLabel(p.ChannelID)
		}
		out = append(out, o)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	for _, o := range out {
		t, _ := time.Parse(time.RFC3339, o.Time)
		head := fmt.Sprintf("[%s] @%s", t.Format("2006-01-02 15:04"), o.User)
		if o.Channel != "" {
			head += " in " + o.Channel
		}
		head += "  id=" + o.ID
		if o.RootID != "" {
			head += " reply-to=" + o.RootID
		}
		if o.Files > 0 {
			head += fmt.Sprintf(" files=%d", o.Files)
		}
		fmt.Println(head)
		for _, line := range strings.Split(strings.TrimRight(o.Message, "\n"), "\n") {
			fmt.Println("  " + line)
		}
	}
	return nil
}

// attachmentText flattens Slack-style attachments (webhook alerts) into text.
func attachmentText(props map[string]any) string {
	atts, _ := props["attachments"].([]any)
	var sb strings.Builder
	for _, a := range atts {
		m, _ := a.(map[string]any)
		for _, k := range []string{"pretext", "title", "text"} {
			if s, _ := m[k].(string); s != "" {
				sb.WriteString("\n▸ " + s)
			}
		}
	}
	return sb.String()
}

// sorted returns a post list oldest-first.
func sorted(pl postList) []post {
	posts := make([]post, 0, len(pl.Posts))
	for _, p := range pl.Posts {
		posts = append(posts, p)
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].CreateAt < posts[j].CreateAt })
	return posts
}

// inOrder returns the posts listed in pl.Order, oldest-first. A channel page's
// Posts map also holds the thread roots of replies on the page, which Order leaves out.
func inOrder(pl postList) []post {
	posts := make([]post, 0, len(pl.Order))
	for i := len(pl.Order) - 1; i >= 0; i-- {
		if p, ok := pl.Posts[pl.Order[i]]; ok {
			posts = append(posts, p)
		}
	}
	return posts
}

// ---- commands ----

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(usage)
		return nil
	}
	cmd := args[0]
	o, err := parseArgs(args[1:])
	if err != nil {
		return err
	}
	need := func(n int, what string) error {
		if len(o.pos) < n {
			return fmt.Errorf("usage: mmdms %s %s", cmd, what)
		}
		return nil
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.init(); err != nil {
		return err
	}
	switch cmd {
	case "whoami":
		fmt.Printf("@%s (%s) on %s, team %s\n", c.me.Username, c.me.ID, c.base, c.team.Name)
		return nil
	case "channels":
		return c.channels(o)
	case "read":
		if err := need(1, "<channel|@user> [-n 30] [--since 2h]"); err != nil {
			return err
		}
		return c.read(o)
	case "thread":
		if err := need(1, "<post-id>"); err != nil {
			return err
		}
		var pl postList
		if err := c.call("GET", "/posts/"+url.PathEscape(o.pos[0])+"/thread", nil, &pl); err != nil {
			return err
		}
		return c.render(sorted(pl), false, o.json)
	case "post":
		if err := need(2, "<channel|@user> <text|-> [--reply <post-id>] [--file <path>]"); err != nil {
			return err
		}
		return c.post(o)
	case "search":
		if err := need(1, "<terms...>"); err != nil {
			return err
		}
		return c.search(strings.Join(o.pos, " "), "", o)
	case "mentions":
		return c.search("@"+c.me.Username, c.me.Username, o)
	case "react":
		if err := need(2, "<post-id> <emoji>"); err != nil {
			return err
		}
		return c.call("POST", "/reactions", map[string]string{
			"user_id": c.me.ID, "post_id": o.pos[0], "emoji_name": strings.Trim(o.pos[1], ":"),
		}, nil)
	}
	return fmt.Errorf("unknown command %q (mmdms --help)", cmd)
}

func (c *client) channels(o opts) error {
	var chans []channel
	if err := c.call("GET", "/users/me/teams/"+c.team.ID+"/channels", nil, &chans); err != nil {
		return err
	}
	var members []struct {
		ChannelID    string `json:"channel_id"`
		MsgCount     int64  `json:"msg_count"`
		MentionCount int64  `json:"mention_count"`
	}
	if err := c.call("GET", "/users/me/teams/"+c.team.ID+"/channels/members", nil, &members); err != nil {
		return err
	}
	var dms []string
	for _, ch := range chans {
		if ch.Type == "D" {
			dms = append(dms, c.dmOther(ch))
		}
	}
	c.loadUsers(dms) // one batch, so label doesn't look DM users up one by one
	seen := map[string][2]int64{}
	for _, m := range members {
		seen[m.ChannelID] = [2]int64{m.MsgCount, m.MentionCount}
	}
	type row struct {
		Channel  string `json:"channel"`
		ID       string `json:"id"`
		Unread   int64  `json:"unread"`
		Mentions int64  `json:"mentions"`
	}
	var rows []row
	for _, ch := range chans {
		r := row{c.label(ch), ch.ID, max(ch.TotalMsgCount-seen[ch.ID][0], 0), seen[ch.ID][1]}
		if o.unread && r.Unread == 0 && r.Mentions == 0 {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Channel < rows[j].Channel })
	if o.json {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	for _, r := range rows {
		fmt.Printf("%-32s unread=%-4d mentions=%-3d id=%s\n", r.Channel, r.Unread, r.Mentions, r.ID)
	}
	return nil
}

func (c *client) read(o opts) error {
	id, err := c.resolve(o.pos[0])
	if err != nil {
		return err
	}
	n := o.n
	if n <= 0 {
		n = 30
	}
	perPage := n
	if o.since > 0 {
		perPage = 200 // ponytail: one page; a --since window with >200 posts shows only the newest 200
	}
	var pl postList
	if err := c.call("GET", fmt.Sprintf("/channels/%s/posts?per_page=%d", id, perPage), nil, &pl); err != nil {
		return err
	}
	posts := inOrder(pl)
	if o.since > 0 {
		cutoff := time.Now().Add(-o.since).UnixMilli()
		posts = posts[sort.Search(len(posts), func(i int) bool { return posts[i].CreateAt >= cutoff }):]
		if o.n > 0 && len(posts) > o.n {
			posts = posts[len(posts)-o.n:]
		}
	}
	return c.render(posts, false, o.json)
}

func (c *client) post(o opts) error {
	id, err := c.resolve(o.pos[0])
	if err != nil {
		return err
	}
	msg := strings.Join(o.pos[1:], " ")
	if msg == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		msg = string(b)
	}
	var fileIDs []string
	if len(o.files) > 0 {
		if fileIDs, err = c.upload(id, o.files); err != nil {
			return err
		}
	}
	var p post
	if err := c.call("POST", "/posts", map[string]any{
		"channel_id": id, "message": msg, "root_id": o.reply, "file_ids": fileIDs,
	}, &p); err != nil {
		return err
	}
	fmt.Printf("posted id=%s %s/%s/pl/%s\n", p.ID, c.base, c.team.Name, p.ID)
	return nil
}

func (c *client) upload(channelID string, paths []string) ([]string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("channel_id", channelID)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		part, err := w.CreateFormFile("files", filepath.Base(p))
		if err == nil {
			_, err = io.Copy(part, f)
		}
		_ = f.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	var res struct {
		FileInfos []struct {
			ID string `json:"id"`
		} `json:"file_infos"`
	}
	if err := c.call("POST", "/files", rawBody{w.FormDataContentType(), buf.Bytes()}, &res); err != nil {
		return nil, err
	}
	ids := make([]string, len(res.FileInfos))
	for i, fi := range res.FileInfos {
		ids[i] = fi.ID
	}
	return ids, nil
}

// search runs a server-side search; mention, if set, keeps only posts that
// actually contain "@mention" (the server also matches the bare word).
func (c *client) search(terms, mention string, o opts) error {
	n := o.n
	if n <= 0 {
		n = 20
	}
	_, offset := time.Now().Zone()
	var pl postList
	if err := c.call("POST", "/teams/"+c.team.ID+"/posts/search", map[string]any{
		"terms": terms, "is_or_search": mention != "", "time_zone_offset": offset,
	}, &pl); err != nil {
		return err
	}
	return c.render(newest(sorted(pl), mention, n), true, o.json)
}

// newest keeps the last n posts (oldest-first input), optionally only those mentioning @mention.
func newest(posts []post, mention string, n int) []post {
	if mention != "" {
		// "@alice" may end in . - _ (trailing punctuation) but not continue as a
		// longer username such as @alicebob or @alice.bob.
		re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(mention) + `[._-]*(?:$|[^a-z0-9._-])`)
		kept := posts[:0]
		for _, p := range posts {
			if re.MatchString(p.Message) {
				kept = append(kept, p)
			}
		}
		posts = kept
	}
	if len(posts) > n {
		posts = posts[len(posts)-n:]
	}
	return posts
}
