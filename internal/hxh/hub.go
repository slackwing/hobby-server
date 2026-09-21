// The chat hub: one per process. Every WebSocket connection is a
// client; the hub persists messages, fans them out to the room's
// members, relays typing (never stored), and announces presence.
//
// Wire format (JSON text frames):
//
//	client → server  {"t":"msg","room":R,"body":S,"image_id":N?}  {"t":"typing","room":R}
//	                 {"t":"read","room":R,"id":N}  (the focused tab showed R up to message N)
//	                 {"t":"ping","focus":true?}  (focus: this tab is the one being looked at)
//	server → client  {"t":"hello","me":U,"contacts":[…],"unread":[{room,count,last_id}…]}
//	                 {"t":"msg","msg":{id,room,sender,body,created_at,image?:{id,width,height}}}
//	                 {"t":"typing","room":R,"user":U}
//	                 {"t":"presence","user":U,"state":S,"last_seen_at":T}
//	                 {"t":"read","room":R,"id":N}  (to the user's OTHER tabs)
//	                 {"t":"pong"}  {"t":"error","code":C,"room":R}
//
// Error codes: room (not yours), body, rate, server, bad, image (a picture
// that is not yours, already sent, or missing), and offline —
// a DM to someone neither online nor away is refused (Andrew,
// 2026-09-19: "you can message online and away people, but not offline").
//
// Presence tiers (spec item 16, reworked 2026-09-21 — Andrew: an instance
// open in a background tab must stay AWAY, never offline, and must not
// look online either). Two signals: FOCUS (a person there: a focused
// tab's ping, a message, typing, a read marker, any authed page request)
// and KEEPALIVE (an instance open: a live socket, or one that dropped
// less than GoneGrace ago — reconnects and throttled tabs must not flap).
//
//	online  = focus in the last minute
//	away    = focus in the last hour, OR an instance open
//	offline = otherwise;  nopass = the account has no password yet.
package hxh

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/slackwing/hobby-server/internal/shared"
)

const (
	rateLimit     = 10.0 // messages per second per connection (bots only)
	sendQueue     = 64   // frames buffered per client before it is dropped as a slow reader
	readTimeout   = 90 * time.Second
	writeTimeout  = 10 * time.Second
	presenceEvery = 20 * time.Second
)

type chatStore interface {
	InsertMessage(room, sender, body string, imageID int64) (Message, error)
	ImageOwnedFree(id int64, user string) (bool, error)
	MarkRead(user, room string, id int64) error
	Unread(user string, after time.Time) ([]RoomUnread, error)
}

type memberSource interface {
	ListMembers() ([]shared.Member, error)
	TouchLastSeen(username string)
}

// Contact is a member plus its presence state.
type Contact struct {
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name"`
	Initial     string     `json:"initial"`
	Color       string     `json:"color"`
	State       string     `json:"state"`
	IsBot       bool       `json:"is_bot"`
	LastSeenAt  *time.Time `json:"last_seen_at"`
}

// GoneGrace: how long after its last socket closed an instance still counts
// as open (a reconnecting or throttled tab must not flap away/offline).
const GoneGrace = 2 * time.Minute

// presenceState: `open` = an instance is open (see the tiers above).
func presenceState(m shared.Member, open bool, now time.Time) string {
	if !m.HasPassword {
		return "nopass"
	}
	if m.LastSeenAt != nil {
		d := now.Sub(*m.LastSeenAt)
		if d < time.Minute {
			return "online"
		}
		if d < time.Hour {
			return "away"
		}
	}
	if open {
		return "away"
	}
	return "offline"
}

// bucket is a token bucket: `rate` tokens/s up to `cap`.
type bucket struct {
	tokens, rate, cap float64
	last              time.Time
}

func (b *bucket) take(now time.Time) bool {
	if !b.last.IsZero() {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.cap {
			b.tokens = b.cap
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

type hubClient struct {
	hub       *Hub
	user      string
	send      chan []byte
	closed    chan struct{}
	closeOnce sync.Once
	limiter   bucket
}

func (c *hubClient) close() { c.closeOnce.Do(func() { close(c.closed) }) }

// enqueue hands a frame to the writer; a client that cannot keep up is
// dropped rather than allowed to stall the hub.
func (c *hubClient) enqueue(data []byte) {
	select {
	case c.send <- data:
	default:
		c.close()
	}
}

func (c *hubClient) sendJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.enqueue(data)
}

type Hub struct {
	mu      sync.Mutex
	store   chatStore
	members memberSource
	clients map[*hubClient]struct{}
	byUser  map[string]map[*hubClient]struct{}
	states  map[string]string    // last presence BROADCAST per user (snapshots never write it)
	gone    map[string]time.Time // when a user's LAST socket closed (keepalive grace)
	now     func() time.Time
	// OnMessage, when set, is called (in its own goroutine) for every
	// message the hub stores — the bot service listens here.
	OnMessage func(Message)
}

func NewHub(store chatStore, members memberSource) *Hub {
	return &Hub{
		store: store, members: members,
		clients: map[*hubClient]struct{}{},
		byUser:  map[string]map[*hubClient]struct{}{},
		states:  map[string]string{},
		gone:    map[string]time.Time{},
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Run re-evaluates presence on a timer until ctx ends.
func (h *Hub) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	h.refreshPresence() // know everyone's tier before the first DM is judged
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.refreshPresence()
		}
	}
}

func (h *Hub) connected(user string) bool {
	return len(h.byUser[user]) > 0
}

// open: is an instance of the user's open — a live socket, or one that
// closed less than GoneGrace ago? Call with h.mu held.
func (h *Hub) open(user string, now time.Time) bool {
	if h.connected(user) {
		return true
	}
	if t, ok := h.gone[user]; ok && now.Sub(t) < GoneGrace {
		return true
	}
	return false
}

// Contacts lists every member with its current presence.
func (h *Hub) Contacts() ([]Contact, error) {
	members, err := h.members.ListMembers()
	if err != nil {
		return nil, err
	}
	return h.contactsOf(members), nil
}

// contactsOf is a presence snapshot of `members`; it records nothing.
func (h *Hub) contactsOf(members []shared.Member) []Contact {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	out := make([]Contact, 0, len(members))
	for _, m := range members {
		state := presenceState(m, h.open(m.Username, now), now)
		out = append(out, Contact{Username: m.Username, DisplayName: m.DisplayName, Initial: m.Initial, Color: m.Color, State: state, IsBot: m.IsBot, LastSeenAt: m.LastSeenAt})
	}
	return out
}

// reachable: may `user` be messaged right now? Online or away — not
// offline, not without a password (Andrew, 2026-09-19: "you can message
// online and away people, but not offline"). A live connection counts
// before the next presence refresh; a member never announced since the
// server started is offline by construction (refreshPresence records
// only the present).
func (h *Hub) reachable(user string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open(user, h.now()) {
		return true
	}
	st := h.states[user]
	return st == "online" || st == "away"
}

// baseline is the state a never-announced user is assumed to have been
// in: someone present now is announced (they were "offline" to everyone),
// someone absent is just recorded.
func baseline(state string) string {
	if state == "online" || state == "away" {
		return "offline"
	}
	return state
}

// refreshPresence recomputes every member's state and announces the
// ones that differ from what was last broadcast. A user never announced
// counts as offline, so the first sign of life is always announced —
// a fetch of the contacts list by the user themself (a bot about to
// speak, say) must never count as "everyone already knows".
func (h *Hub) refreshPresence() {
	members, err := h.members.ListMembers()
	if err != nil {
		log.Printf("[hxh chat] presence refresh error: %v", err)
		return
	}
	h.mu.Lock()
	now := h.now()
	type change struct {
		user, state string
		seen        *time.Time
	}
	var changes []change
	for _, m := range members {
		state := presenceState(m, h.open(m.Username, now), now)
		prev, known := h.states[m.Username]
		if !known {
			prev = baseline(state)
		}
		if prev != state {
			h.states[m.Username] = state
			changes = append(changes, change{m.Username, state, m.LastSeenAt})
		}
	}
	h.mu.Unlock()
	for _, c := range changes {
		h.broadcastAll(map[string]any{"t": "presence", "user": c.user, "state": c.state, "last_seen_at": c.seen})
	}
}

// announce recomputes one user's presence (after connect/disconnect).
func (h *Hub) announce(user string) {
	members, err := h.members.ListMembers()
	if err != nil {
		return
	}
	for _, m := range members {
		if m.Username != user {
			continue
		}
		h.mu.Lock()
		state := presenceState(m, h.open(user, h.now()), h.now())
		prev, known := h.states[user]
		if !known {
			prev = baseline(state)
		}
		changed := prev != state
		h.states[user] = state
		h.mu.Unlock()
		if changed {
			h.broadcastAll(map[string]any{"t": "presence", "user": user, "state": state, "last_seen_at": m.LastSeenAt})
		}
		return
	}
}

// add registers a connection for `user`: it gets the hello frame, and
// everyone learns the user is online.
func (h *Hub) add(user string) *hubClient {
	c := &hubClient{hub: h, user: user, send: make(chan []byte, sendQueue), closed: make(chan struct{}),
		limiter: bucket{tokens: rateLimit, rate: rateLimit, cap: rateLimit}}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	if h.byUser[user] == nil {
		h.byUser[user] = map[*hubClient]struct{}{}
	}
	h.byUser[user][c] = struct{}{}
	h.mu.Unlock()
	// a socket is an instance, not a person: connecting touches nothing (a page load already did, over HTTP)
	members, err := h.members.ListMembers()
	if err != nil {
		log.Printf("[hxh chat] hello members error: %v", err)
	}
	contacts := h.contactsOf(members)
	unread := []RoomUnread{}
	for _, m := range members {
		if m.Username != user {
			continue
		}
		var after time.Time
		if m.ActivatedAt != nil {
			after = *m.ActivatedAt
		}
		if u, err := h.store.Unread(user, after); err != nil {
			log.Printf("[hxh chat] hello unread error: %v", err)
		} else if u != nil {
			unread = u
		}
		break
	}
	c.sendJSON(map[string]any{"t": "hello", "me": user, "contacts": contacts, "unread": unread})
	h.announce(user)
	return c
}

func (h *Hub) remove(c *hubClient) {
	c.close()
	h.mu.Lock()
	delete(h.clients, c)
	if set := h.byUser[c.user]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byUser, c.user)
			h.gone[c.user] = h.now()
		}
	}
	h.mu.Unlock()
	h.announce(c.user)
}

// broadcastUser sends v to every connection of `user` but `except`: a
// read marker made in one tab reaches the user's other tabs.
func (h *Hub) broadcastUser(user string, v any, except *hubClient) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.byUser[user] {
		if c != except {
			c.enqueue(data)
		}
	}
}

func (h *Hub) broadcastAll(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	targets := make([]*hubClient, 0, len(h.clients))
	for c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.Unlock()
	for _, c := range targets {
		c.enqueue(data)
	}
}

// broadcastRoom sends to everyone who may see the room, except the
// connections of `except` (e.g. the typist's own).
func (h *Hub) broadcastRoom(room string, v any, except string) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	parties, ok := roomParties(room)
	if !ok {
		return
	}
	h.mu.Lock()
	var targets []*hubClient
	if parties == nil {
		for c := range h.clients {
			if c.user != except {
				targets = append(targets, c)
			}
		}
	} else {
		for _, u := range parties {
			if u == except {
				continue
			}
			for c := range h.byUser[u] {
				targets = append(targets, c)
			}
		}
	}
	h.mu.Unlock()
	for _, c := range targets {
		c.enqueue(data)
	}
}

type inFrame struct {
	T       string `json:"t"`
	Room    string `json:"room"`
	Body    string `json:"body"`
	ID      int64  `json:"id"`
	ImageID int64  `json:"image_id"` // msg: a picture uploaded by this user, not yet on a message
	Focus   bool   `json:"focus"`    // ping: the sending tab is focused and visible — a person is there
}

func (c *hubClient) fail(code, room string) {
	c.sendJSON(map[string]any{"t": "error", "code": code, "room": room})
}

// handle processes one inbound frame from `c`.
func (h *Hub) handle(c *hubClient, data []byte) {
	var f inFrame
	if err := json.Unmarshal(data, &f); err != nil {
		c.fail("bad", "")
		return
	}
	switch f.T {
	case "ping":
		if f.Focus { // only a looked-at tab's heartbeat is a sign of a person
			h.members.TouchLastSeen(c.user)
		}
		c.sendJSON(map[string]any{"t": "pong"})
	case "typing":
		if !canUseRoom(c.user, f.Room) {
			c.fail("room", f.Room)
			return
		}
		h.members.TouchLastSeen(c.user)
		h.broadcastRoom(f.Room, map[string]any{"t": "typing", "room": f.Room, "user": c.user}, c.user)
	case "read":
		// the focused tab's active window showed the room up to message ID
		if !canUseRoom(c.user, f.Room) {
			c.fail("room", f.Room)
			return
		}
		h.members.TouchLastSeen(c.user)
		if f.ID <= 0 {
			c.fail("bad", f.Room)
			return
		}
		if err := h.store.MarkRead(c.user, f.Room, f.ID); err != nil {
			log.Printf("[hxh chat] read error: %v", err)
			c.fail("server", f.Room)
			return
		}
		h.broadcastUser(c.user, map[string]any{"t": "read", "room": f.Room, "id": f.ID}, c)
	case "msg":
		if !canUseRoom(c.user, f.Room) {
			c.fail("room", f.Room)
			return
		}
		if other := DMPartner(c.user, f.Room); other != "" && !h.reachable(other) {
			c.fail("offline", f.Room)
			return
		}
		h.members.TouchLastSeen(c.user)
		body := strings.TrimSpace(f.Body)
		if (body == "" && f.ImageID <= 0) || utf8.RuneCountInString(body) > MaxBody {
			c.fail("body", f.Room)
			return
		}
		if f.ImageID > 0 {
			ok, err := h.store.ImageOwnedFree(f.ImageID, c.user)
			if err != nil {
				log.Printf("[hxh chat] image check error: %v", err)
				c.fail("server", f.Room)
				return
			}
			if !ok { // not yours, already sent, or no such picture
				c.fail("image", f.Room)
				return
			}
		}
		if !c.limiter.take(h.now()) {
			c.fail("rate", f.Room)
			return
		}
		m, err := h.store.InsertMessage(f.Room, c.user, body, f.ImageID)
		if err != nil {
			log.Printf("[hxh chat] insert error: %v", err)
			c.fail("server", f.Room)
			return
		}
		h.broadcastRoom(f.Room, map[string]any{"t": "msg", "msg": m}, "")
		if h.OnMessage != nil {
			go h.OnMessage(m)
		}
	default:
		c.fail("bad", f.Room)
	}
}

// HandleWS upgrades a member's request to a WebSocket and pumps it
// through the hub until either side hangs up. Mount it OUTSIDE any
// per-request timeout middleware — a connection lives for hours.
func (c *Chat) HandleWS() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := c.member(r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			log.Printf("[hxh chat] accept error: %v", err)
			return
		}
		cl := c.Hub.add(user)
		ctx := context.Background()
		go func() {
			for {
				select {
				case data := <-cl.send:
					wctx, cancel := context.WithTimeout(ctx, writeTimeout)
					err := conn.Write(wctx, websocket.MessageText, data)
					cancel()
					if err != nil {
						cl.close()
						return
					}
				case <-cl.closed:
					return
				}
			}
		}()
	loop:
		for {
			rctx, cancel := context.WithTimeout(ctx, readTimeout)
			typ, data, err := conn.Read(rctx)
			cancel()
			if err != nil {
				break
			}
			if typ == websocket.MessageText {
				c.Hub.handle(cl, data)
			}
			select {
			case <-cl.closed:
				break loop
			default:
			}
		}
		c.Hub.remove(cl)
		_ = conn.Close(websocket.StatusNormalClosure, "bye")
	}
}
