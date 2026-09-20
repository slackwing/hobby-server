package hxh

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slackwing/hobby-server/internal/shared"
)

// ---- fakes ----

type memStore struct {
	mu     sync.Mutex
	msgs   []Message
	next   int64
	reads  map[string]int64 // user|room → last read id
	images map[int64]*memImage
}

type memImage struct {
	sender string
	used   bool
}

func (s *memStore) InsertMessage(room, sender, body string, imageID int64) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	m := Message{ID: s.next, Room: room, Sender: sender, Body: body, CreatedAt: time.Now()}
	if imageID > 0 {
		m.Image = &ImageRef{ID: imageID, Width: 640, Height: 480}
		if s.images != nil {
			s.images[imageID].used = true
		}
	}
	s.msgs = append(s.msgs, m)
	return m, nil
}

func (s *memStore) ImageOwnedFree(id int64, user string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	im, ok := s.images[id]
	return ok && im.sender == user && !im.used, nil
}

func (s *memStore) addImage(id int64, sender string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.images == nil {
		s.images = map[int64]*memImage{}
	}
	s.images[id] = &memImage{sender: sender}
}

func (s *memStore) MarkRead(user, room string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reads == nil {
		s.reads = map[string]int64{}
	}
	if id > s.reads[user+"|"+room] {
		s.reads[user+"|"+room] = id
	}
	return nil
}

// Unread mirrors the SQL: others' messages after `after`, past the marker, in rooms of mine; oldest room first.
func (s *memStore) Unread(user string, after time.Time) ([]RoomUnread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	agg := map[string]*RoomUnread{}
	var order []string
	for _, m := range s.msgs {
		if m.Sender == user || !m.CreatedAt.After(after) || (m.Room != RoomGlobal && DMPartner(user, m.Room) == "") {
			continue
		}
		if m.ID <= s.reads[user+"|"+m.Room] {
			continue
		}
		u := agg[m.Room]
		if u == nil {
			u = &RoomUnread{Room: m.Room}
			agg[m.Room] = u
			order = append(order, m.Room)
		}
		u.Count++
		u.LastID = m.ID
	}
	out := []RoomUnread{}
	for _, r := range order {
		out = append(out, *agg[r])
	}
	return out, nil
}

type memMembers struct {
	mu      sync.Mutex
	members []shared.Member
	touched []string
}

func (m *memMembers) ListMembers() ([]shared.Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]shared.Member, len(m.members))
	copy(out, m.members)
	return out, nil
}
func (m *memMembers) TouchLastSeen(u string) {
	m.mu.Lock()
	m.touched = append(m.touched, u)
	m.mu.Unlock()
}
func (m *memMembers) seen(u string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.members {
		if m.members[i].Username == u {
			t := at
			m.members[i].LastSeenAt = &t
		}
	}
}

func member(u string, pw bool) shared.Member {
	return shared.Member{Username: u, DisplayName: u, Initial: "X", Color: "#123456", HasPassword: pw}
}

func newTestHub() (*Hub, *memStore, *memMembers, *time.Time) {
	store := &memStore{}
	members := &memMembers{members: []shared.Member{member("andrew", true), member("abi", true), member("newbie", false)}}
	h := NewHub(store, members)
	now := time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	return h, store, members, &now
}

// next pops the client's next frame (or fails after 200 ms).
func next(t *testing.T, c *hubClient) map[string]any {
	t.Helper()
	select {
	case data := <-c.send:
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("bad frame %s: %v", data, err)
		}
		return m
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no frame")
	}
	return nil
}

func none(t *testing.T, c *hubClient) {
	t.Helper()
	select {
	case data := <-c.send:
		t.Fatalf("unexpected frame %s", data)
	case <-time.After(30 * time.Millisecond):
	}
}

func frame(c *hubClient, v any) {
	data, _ := json.Marshal(v)
	c.hub.handle(c, data)
}

// ---- tests ----

func TestRooms(t *testing.T) {
	if DMRoom("b", "a") != "dm:a:b" || DMRoom("a", "b") != "dm:a:b" {
		t.Fatal("DMRoom must sort")
	}
	cases := []struct {
		user, room string
		ok         bool
	}{
		{"a", "global", true}, {"a", "dm:a:b", true}, {"b", "dm:a:b", true}, {"c", "dm:a:b", false},
		{"a", "dm:b:a", false}, {"a", "dm:a:", false}, {"a", "secret", false}, {"a", "", false},
	}
	for _, c := range cases {
		if got := canUseRoom(c.user, c.room); got != c.ok {
			t.Errorf("canUseRoom(%q, %q) = %v, want %v", c.user, c.room, got, c.ok)
		}
	}
}

func TestPresenceState(t *testing.T) {
	now := time.Now()
	m := member("x", true)
	if presenceState(m, true, now) != "online" {
		t.Error("connected → online")
	}
	if presenceState(m, false, now) != "offline" {
		t.Error("never seen → offline")
	}
	at := now.Add(-30 * time.Second)
	m.LastSeenAt = &at
	if presenceState(m, false, now) != "online" {
		t.Error("< 1 min → online")
	}
	at = now.Add(-30 * time.Minute)
	if presenceState(m, false, now) != "away" {
		t.Error("< 1 h → away")
	}
	at = now.Add(-2 * time.Hour)
	if presenceState(m, false, now) != "offline" {
		t.Error("> 1 h → offline")
	}
	m.HasPassword = false
	if presenceState(m, true, now) != "nopass" {
		t.Error("no password → nopass even when connected")
	}
}

func TestHelloAndPresence(t *testing.T) {
	h, _, members, _ := newTestHub()
	andrew := h.add("andrew")
	hello := next(t, andrew)
	if hello["t"] != "hello" || hello["me"] != "andrew" {
		t.Fatalf("want hello, got %v", hello)
	}
	contacts := hello["contacts"].([]any)
	if len(contacts) != 3 {
		t.Fatalf("want 3 contacts, got %d", len(contacts))
	}
	states := map[string]string{}
	for _, c := range contacts {
		m := c.(map[string]any)
		states[m["username"].(string)] = m["state"].(string)
	}
	if states["andrew"] != "online" || states["abi"] != "offline" || states["newbie"] != "nopass" {
		t.Fatalf("states %v", states)
	}
	// andrew's own connect announced him online (to himself too — he is connected)
	p := next(t, andrew)
	if p["t"] != "presence" || p["user"] != "andrew" || p["state"] != "online" {
		t.Fatalf("want presence online, got %v", p)
	}
	if len(members.touched) == 0 || members.touched[0] != "andrew" {
		t.Error("connect must touch last_seen")
	}
	// abi connects: andrew hears it
	abi := h.add("abi")
	next(t, abi) // hello
	p = next(t, andrew)
	if p["user"] != "abi" || p["state"] != "online" {
		t.Fatalf("andrew should hear abi online, got %v", p)
	}
	next(t, abi) // abi's own presence
	// abi hangs up with no recent activity: offline
	h.remove(abi)
	p = next(t, andrew)
	if p["user"] != "abi" || p["state"] != "offline" {
		t.Fatalf("want abi offline, got %v", p)
	}
	none(t, andrew)
}

func TestOwnContactsFetchDoesNotSwallowPresence(t *testing.T) {
	h, _, members, now := newTestHub()
	andrew := h.add("andrew")
	next(t, andrew)
	next(t, andrew)
	// abi's bot logs in and fetches the contacts list (touching last_seen) — a snapshot, not a broadcast
	members.seen("abi", now.Add(-2*time.Second))
	if _, err := h.Contacts(); err != nil {
		t.Fatal(err)
	}
	none(t, andrew)
	// then she connects: andrew must still hear her come online
	abi := h.add("abi")
	next(t, abi)
	p := next(t, andrew)
	if p["t"] != "presence" || p["user"] != "abi" || p["state"] != "online" {
		t.Fatalf("want abi online, got %v", p)
	}
}

func TestPresenceRefreshTiers(t *testing.T) {
	h, _, members, now := newTestHub()
	andrew := h.add("andrew")
	next(t, andrew)
	next(t, andrew)
	members.seen("abi", now.Add(-10*time.Second))
	h.refreshPresence()
	p := next(t, andrew)
	if p["user"] != "abi" || p["state"] != "online" {
		t.Fatalf("recent activity → online, got %v", p)
	}
	*now = now.Add(5 * time.Minute)
	h.refreshPresence()
	p = next(t, andrew)
	if p["state"] != "away" {
		t.Fatalf("5 min later → away, got %v", p)
	}
	h.refreshPresence()
	none(t, andrew) // unchanged: nothing announced
	*now = now.Add(2 * time.Hour)
	h.refreshPresence()
	if p = next(t, andrew); p["state"] != "offline" {
		t.Fatalf("2 h later → offline, got %v", p)
	}
}

func drain(cs ...*hubClient) {
	for _, c := range cs {
		for len(c.send) > 0 {
			<-c.send
		}
	}
}

// unreadOf renders a hello's unread list as "room:count@last_id …".
func unreadOf(hello map[string]any) string {
	var parts []string
	for _, x := range hello["unread"].([]any) {
		u := x.(map[string]any)
		parts = append(parts, fmt.Sprintf("%s:%v@%v", u["room"], u["count"], u["last_id"]))
	}
	return strings.Join(parts, " ")
}

func TestHelloCarriesUnreadAndReadsAreSharedAcrossTabs(t *testing.T) {
	h, store, _, _ := newTestHub()
	dm := DMRoom("abi", "andrew")
	store.InsertMessage(RoomGlobal, "abi", "one", 0)
	store.InsertMessage(RoomGlobal, "abi", "two", 0)
	m3, _ := store.InsertMessage(dm, "abi", "psst", 0)
	store.InsertMessage(DMRoom("abi", "newbie"), "abi", "not yours", 0)
	store.InsertMessage(RoomGlobal, "andrew", "mine", 0) // own messages never count
	andrew := h.add("andrew")
	hello := next(t, andrew)
	if hello["t"] != "hello" {
		t.Fatalf("hello first, got %v", hello)
	}
	if got := unreadOf(hello); got != "global:2@2 dm:abi:andrew:1@3" {
		t.Fatalf("unread per room, oldest first: %s", got)
	}
	drain(andrew)
	// the focused tab read global up to 2: no echo to itself…
	frame(andrew, map[string]any{"t": "read", "room": RoomGlobal, "id": 2})
	none(t, andrew)
	// …and the next hello no longer lists global
	tab2 := h.add("andrew")
	hello2 := next(t, tab2)
	if got := unreadOf(hello2); got != "dm:abi:andrew:1@3" {
		t.Fatalf("global was read: %s", got)
	}
	drain(andrew, tab2)
	// a read in the second tab reaches the first (and nobody else)
	abi := h.add("abi")
	drain(andrew, tab2, abi)
	frame(tab2, map[string]any{"t": "read", "room": dm, "id": m3.ID})
	r := next(t, andrew)
	if r["t"] != "read" || r["room"] != dm || r["id"] != float64(m3.ID) {
		t.Fatalf("other tab told, got %v", r)
	}
	none(t, tab2)
	none(t, abi)
	// markers never move backwards; foreign rooms and bad ids are refused
	frame(tab2, map[string]any{"t": "read", "room": dm, "id": 1})
	u, _ := store.Unread("andrew", time.Time{})
	if len(u) != 0 {
		t.Fatalf("still read after a stale marker, got %v", u)
	}
	drain(andrew)
	frame(tab2, map[string]any{"t": "read", "room": DMRoom("abi", "newbie"), "id": 4})
	if e := next(t, tab2); e["code"] != "room" {
		t.Fatalf("foreign room refused, got %v", e)
	}
	frame(tab2, map[string]any{"t": "read", "room": RoomGlobal, "id": 0})
	if e := next(t, tab2); e["code"] != "bad" {
		t.Fatalf("id 0 refused, got %v", e)
	}
}

func TestDMNeedsAReachablePartner(t *testing.T) {
	h, _, members, now := newTestHub()
	andrew := h.add("andrew")
	h.refreshPresence() // abi never seen: offline, and so never recorded
	drain(andrew)
	room := DMRoom("andrew", "abi")
	say := func() map[string]any {
		frame(andrew, map[string]any{"t": "msg", "room": room, "body": "psst"})
		return next(t, andrew)
	}
	if e := say(); e["t"] != "error" || e["code"] != "offline" {
		t.Fatalf("offline partner refused, got %v", e)
	}
	members.seen("abi", now.Add(-30*time.Minute)) // away
	h.refreshPresence()
	drain(andrew)
	if m := say(); m["t"] != "msg" {
		t.Fatalf("away partner reachable, got %v", m)
	}
	*now = now.Add(2 * time.Hour) // abi drifts offline
	h.refreshPresence()
	drain(andrew)
	if e := say(); e["code"] != "offline" {
		t.Fatalf("offline again, got %v", e)
	}
	abi := h.add("abi") // connected: online at once, before any refresh
	drain(andrew, abi)
	if m := say(); m["t"] != "msg" {
		t.Fatalf("connected partner reachable, got %v", m)
	}
	drain(abi)
	frame(andrew, map[string]any{"t": "msg", "room": DMRoom("andrew", "newbie"), "body": "hi"})
	if e := next(t, andrew); e["code"] != "offline" {
		t.Fatalf("no password = unreachable, got %v", e)
	}
	frame(andrew, map[string]any{"t": "msg", "room": RoomGlobal, "body": "all"})
	if m := next(t, andrew); m["t"] != "msg" {
		t.Fatalf("global needs nobody, got %v", m)
	}
}

func TestAPictureRidesOneMessage(t *testing.T) {
	h, store, _, _ := newTestHub()
	andrew, abi := h.add("andrew"), h.add("abi")
	drain(andrew, abi)
	store.addImage(7, "andrew")
	store.addImage(8, "abi")
	// a picture alone is a message; the fan-out carries its size
	frame(andrew, map[string]any{"t": "msg", "room": RoomGlobal, "body": "", "image_id": 7})
	m := next(t, andrew)
	img, _ := m["msg"].(map[string]any)["image"].(map[string]any)
	if m["t"] != "msg" || img["id"] != float64(7) || img["width"] != float64(640) {
		t.Fatalf("picture message, got %v", m)
	}
	drain(abi)
	// the same picture cannot be sent twice; someone else's picture, or none, is refused; text is still required without one
	frame(andrew, map[string]any{"t": "msg", "room": RoomGlobal, "body": "again", "image_id": 7})
	if e := next(t, andrew); e["code"] != "image" {
		t.Fatalf("a used picture is refused, got %v", e)
	}
	frame(andrew, map[string]any{"t": "msg", "room": RoomGlobal, "body": "hers", "image_id": 8})
	if e := next(t, andrew); e["code"] != "image" {
		t.Fatalf("someone else's picture is refused, got %v", e)
	}
	frame(andrew, map[string]any{"t": "msg", "room": RoomGlobal, "body": "ghost", "image_id": 99})
	if e := next(t, andrew); e["code"] != "image" {
		t.Fatalf("a missing picture is refused, got %v", e)
	}
	frame(andrew, map[string]any{"t": "msg", "room": RoomGlobal, "body": "  "})
	if e := next(t, andrew); e["code"] != "body" {
		t.Fatalf("no text and no picture is nothing, got %v", e)
	}
	none(t, abi)
}

func TestGlobalMessageFanOut(t *testing.T) {
	h, store, _, _ := newTestHub()
	andrew, abi := h.add("andrew"), h.add("abi")
	for _, c := range []*hubClient{andrew, abi} {
		for len(c.send) > 0 {
			<-c.send
		}
	}
	frame(andrew, map[string]any{"t": "msg", "room": "global", "body": "  hello all  "})
	for _, c := range []*hubClient{andrew, abi} {
		f := next(t, c)
		if f["t"] != "msg" {
			t.Fatalf("want msg, got %v", f)
		}
		m := f["msg"].(map[string]any)
		if m["body"] != "hello all" || m["sender"] != "andrew" || m["room"] != "global" || m["id"].(float64) != 1 {
			t.Fatalf("bad msg %v", m)
		}
	}
	if len(store.msgs) != 1 {
		t.Fatal("message must be stored")
	}
}

func TestDMOnlyReachesParties(t *testing.T) {
	h, _, _, _ := newTestHub()
	andrew, abi, newbie := h.add("andrew"), h.add("abi"), h.add("newbie")
	for _, c := range []*hubClient{andrew, abi, newbie} {
		for len(c.send) > 0 {
			<-c.send
		}
	}
	room := DMRoom("andrew", "abi")
	frame(andrew, map[string]any{"t": "msg", "room": room, "body": "psst"})
	if next(t, abi)["t"] != "msg" || next(t, andrew)["t"] != "msg" {
		t.Fatal("both parties get the DM")
	}
	none(t, newbie)
	frame(newbie, map[string]any{"t": "msg", "room": room, "body": "let me in"})
	e := next(t, newbie)
	if e["t"] != "error" || e["code"] != "room" {
		t.Fatalf("outsider must be refused, got %v", e)
	}
	none(t, abi)
	none(t, andrew)
}

func TestTypingIsEphemeral(t *testing.T) {
	h, store, _, _ := newTestHub()
	andrew, abi := h.add("andrew"), h.add("abi")
	for _, c := range []*hubClient{andrew, abi} {
		for len(c.send) > 0 {
			<-c.send
		}
	}
	frame(andrew, map[string]any{"t": "typing", "room": "global"})
	f := next(t, abi)
	if f["t"] != "typing" || f["user"] != "andrew" || f["room"] != "global" {
		t.Fatalf("want typing, got %v", f)
	}
	none(t, andrew) // not echoed to the typist
	if len(store.msgs) != 0 {
		t.Fatal("typing must never be stored")
	}
}

func TestBodyRulesAndRateLimit(t *testing.T) {
	h, store, _, now := newTestHub()
	andrew := h.add("andrew")
	for len(andrew.send) > 0 {
		<-andrew.send
	}
	frame(andrew, map[string]any{"t": "msg", "room": "global", "body": "   "})
	if e := next(t, andrew); e["code"] != "body" {
		t.Fatalf("blank body refused, got %v", e)
	}
	long := make([]rune, MaxBody+1)
	for i := range long {
		long[i] = 'x'
	}
	frame(andrew, map[string]any{"t": "msg", "room": "global", "body": string(long)})
	if e := next(t, andrew); e["code"] != "body" {
		t.Fatalf("overlong body refused, got %v", e)
	}
	for i := 0; i < 10; i++ {
		frame(andrew, map[string]any{"t": "msg", "room": "global", "body": "spam"})
		if f := next(t, andrew); f["t"] != "msg" {
			t.Fatalf("message %d should pass, got %v", i, f)
		}
	}
	frame(andrew, map[string]any{"t": "msg", "room": "global", "body": "spam"})
	if e := next(t, andrew); e["code"] != "rate" {
		t.Fatalf("11th in a burst must be rate limited, got %v", e)
	}
	if len(store.msgs) != 10 {
		t.Fatalf("stored %d, want 10", len(store.msgs))
	}
	*now = now.Add(time.Second) // a second later the bucket has refilled
	frame(andrew, map[string]any{"t": "msg", "room": "global", "body": "ok again"})
	if f := next(t, andrew); f["t"] != "msg" {
		t.Fatalf("after refill should pass, got %v", f)
	}
}

func TestPingPongAndBadFrames(t *testing.T) {
	h, _, members, _ := newTestHub()
	andrew := h.add("andrew")
	for len(andrew.send) > 0 {
		<-andrew.send
	}
	h.handle(andrew, []byte(`{"t":"ping"}`))
	if next(t, andrew)["t"] != "pong" {
		t.Fatal("ping → pong")
	}
	if members.touched[len(members.touched)-1] != "andrew" {
		t.Error("ping must count as activity")
	}
	h.handle(andrew, []byte(`not json`))
	if next(t, andrew)["code"] != "bad" {
		t.Fatal("garbage → error bad")
	}
	h.handle(andrew, []byte(`{"t":"dance"}`))
	if next(t, andrew)["code"] != "bad" {
		t.Fatal("unknown type → error bad")
	}
}

func TestSlowReaderIsDropped(t *testing.T) {
	h, _, _, _ := newTestHub()
	andrew := h.add("andrew")
	for i := 0; i < sendQueue+5; i++ {
		andrew.sendJSON(map[string]any{"t": "pong"})
	}
	select {
	case <-andrew.closed:
	default:
		t.Fatal("a client whose queue overflows must be closed")
	}
}

func TestNormalizeRuns(t *testing.T) {
	runs, err := NormalizeRuns([]byte(`[{"t":"hi ","b":true,"font":"px","size":3,"color":"#FF0000","bg":"#00ff00"},{"t":""},{"t":"x","font":"comic","size":9,"color":"red","bg":"#12"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("empty runs dropped; got %d", len(runs))
	}
	if runs[0].Color != "#ff0000" || runs[0].BG != "#00ff00" || runs[0].Font != "px" || runs[0].Size != 3 || !runs[0].B {
		t.Fatalf("valid styles kept: %+v", runs[0])
	}
	if runs[1].Font != "" || runs[1].Size != 0 || runs[1].Color != "" || runs[1].BG != "" {
		t.Fatalf("invalid styles dropped: %+v", runs[1])
	}
	if _, err := NormalizeRuns([]byte(`{"t":"x"}`)); err == nil {
		t.Error("non-array rejected")
	}
	big := make([]byte, 0, ProfileLimit+40)
	big = append(big, `[{"t":"`...)
	for i := 0; i < ProfileLimit+1; i++ {
		big = append(big, 'a')
	}
	big = append(big, `"}]`...)
	if _, err := NormalizeRuns(big); err == nil {
		t.Error("over the character limit rejected")
	}
	exact := make([]byte, 0, ProfileLimit+40)
	exact = append(exact, `[{"t":"`...)
	for i := 0; i < ProfileLimit; i++ {
		exact = append(exact, 'é') // multi-byte: the limit counts characters, not bytes
	}
	exact = append(exact, `"}]`...)
	if _, err := NormalizeRuns([]byte(string(exact))); err != nil {
		t.Errorf("exactly the limit is fine: %v", err)
	}
}
