// The Beetle messenger's data layer and HTTP endpoints (spec: feathers
// foundry/website/docs/HXH_CHAT.md). Realtime traffic — messages,
// typing, presence — goes over the WebSocket hub in hub.go; these are
// the request/response pieces: the contacts list, history, profiles.
//
// Every endpoint requires a shared-auth session whose user holds ANY
// role on website "hxh" (guests chat too; admin is not required).
package hxh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/slackwing/hobby-server/internal/shared"
)

const (
	ChatWebsite  = "hxh"
	RoomGlobal   = "global"
	MaxBody      = 2000 // characters per message
	HistoryLimit = 100  // messages a window shows
	ProfileLimit = 1024 // characters of profile text (AIM-sized)
	maxRuns      = 500
)

// Message is one chat line as the client sees it.
type Message struct {
	ID        int64     `json:"id"`
	Room      string    `json:"room"`
	Sender    string    `json:"sender"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// DMRoom names the private room between two users, whichever order
// they are given in.
func DMRoom(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "dm:" + a + ":" + b
}

// roomParties returns who may use a room: nil (everyone) for the global
// room, the two usernames for a DM, ok=false for anything else.
func roomParties(room string) (parties []string, ok bool) {
	if room == RoomGlobal {
		return nil, true
	}
	if strings.HasPrefix(room, "dm:") {
		parts := strings.Split(room, ":")
		if len(parts) == 3 && parts[1] != "" && parts[2] != "" && parts[1] < parts[2] {
			return parts[1:], true
		}
	}
	return nil, false
}

// DMPartner is the other party of a DM room `user` belongs to, or "" for
// the global room and for rooms `user` has no business in.
func DMPartner(user, room string) string {
	parties, ok := roomParties(room)
	if !ok || len(parties) != 2 {
		return ""
	}
	switch user {
	case parties[0]:
		return parties[1]
	case parties[1]:
		return parties[0]
	}
	return ""
}

// canUseRoom: global is open to every member; a DM only to its two parties.
func canUseRoom(user, room string) bool {
	p, ok := roomParties(room)
	if !ok {
		return false
	}
	if p == nil {
		return true
	}
	return p[0] == user || p[1] == user
}

// ---------- store ----------

func (s *Store) InsertMessage(room, sender, body string) (Message, error) {
	ctx, cancel := withCtx()
	defer cancel()
	m := Message{Room: room, Sender: sender, Body: body}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO hxh_chat_message (room, sender, body) VALUES ($1, $2, $3)
		RETURNING id, created_at
	`, room, sender, body).Scan(&m.ID, &m.CreatedAt)
	return m, err
}

// RoomUnread is what a member has not read in one room.
type RoomUnread struct {
	Room   string `json:"room"`
	Count  int    `json:"count"`
	LastID int64  `json:"last_id"`
}

// MarkRead records that `user` has read `room` up to message `id`. The
// marker never moves backwards (two tabs may report in either order).
func (s *Store) MarkRead(user, room string, id int64) error {
	ctx, cancel := withCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO hxh_chat_read (username, room, last_read_id) VALUES ($1, $2, $3)
		ON CONFLICT (username, room) DO UPDATE
		SET last_read_id = GREATEST(hxh_chat_read.last_read_id, EXCLUDED.last_read_id), updated_at = NOW()
	`, user, room, id)
	return err
}

// Unread counts, per room `user` belongs to (global and their DMs), the
// messages by others written after `after` (the viewer's activation, as
// History) and past the user's read marker. Rooms with nothing unread
// are absent. Oldest room first, so windows open in arrival order.
func (s *Store) Unread(user string, after time.Time) ([]RoomUnread, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT m.room, COUNT(*), MAX(m.id)
		FROM hxh_chat_message m
		LEFT JOIN hxh_chat_read r ON r.username = $1 AND r.room = m.room
		WHERE (m.room = 'global' OR split_part(m.room, ':', 2) = $1 OR split_part(m.room, ':', 3) = $1)
		  AND m.sender <> $1 AND m.deleted_at IS NULL AND m.created_at > $2
		  AND m.id > COALESCE(r.last_read_id, 0)
		GROUP BY m.room ORDER BY MAX(m.id)
	`, user, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoomUnread{}
	for rows.Next() {
		var u RoomUnread
		if err := rows.Scan(&u.Room, &u.Count, &u.LastID); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// History returns the newest `limit` messages of a room written after
// `after` (the viewer's activation — item 13), oldest first. deleted_at
// is a landed column (unsend was dropped 2026-09-18 as anachronistic —
// Andrew); nothing sets it any more.
func (s *Store) History(room string, after time.Time, limit int) ([]Message, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT id, room, sender, body, created_at FROM hxh_chat_message
		WHERE room = $1 AND deleted_at IS NULL AND created_at > $2
		ORDER BY id DESC LIMIT $3
	`, room, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Room, &m.Sender, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (s *Store) GetProfile(username string) (runs json.RawMessage, updated time.Time, ok bool, err error) {
	ctx, cancel := withCtx()
	defer cancel()
	err = s.pool.QueryRow(ctx, `SELECT runs, updated_at FROM hxh_chat_profile WHERE username = $1`, username).Scan(&runs, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return json.RawMessage("[]"), time.Time{}, false, nil
	}
	return runs, updated, err == nil, err
}

func (s *Store) PutProfile(username string, runs json.RawMessage) error {
	ctx, cancel := withCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO hxh_chat_profile (username, runs, updated_at) VALUES ($1, $2, NOW())
		ON CONFLICT (username) DO UPDATE SET runs = EXCLUDED.runs, updated_at = NOW()
	`, username, runs)
	return err
}

// ---------- profile runs ----------

// Run is one span of styled profile text — our own format, mirrored by
// apps/chat/runs.js in feathers. Never HTML: the client renders runs
// with textContent and an allowlisted style, so nothing here can carry
// markup.
type Run struct {
	T     string `json:"t"`
	B     bool   `json:"b,omitempty"`
	I     bool   `json:"i,omitempty"`
	U     bool   `json:"u,omitempty"`
	Font  string `json:"font,omitempty"`  // dot | px | serif | sans | mono | cursive
	Size  int    `json:"size,omitempty"`  // 1..7 like <font size>, 0 = default
	Color string `json:"color,omitempty"` // #rrggbb
	BG    string `json:"bg,omitempty"`    // #rrggbb highlight
}

var profileFonts = map[string]bool{"dot": true, "px": true, "serif": true, "sans": true, "mono": true, "cursive": true}
var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// NormalizeRuns validates and cleans a profile: unknown fonts/sizes/
// colours are dropped (not rejected), empty runs removed, and the text
// as a whole must fit ProfileLimit characters.
func NormalizeRuns(raw []byte) ([]Run, error) {
	var in []Run
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errors.New("runs must be a JSON array")
	}
	if len(in) > maxRuns {
		return nil, fmt.Errorf("too many runs (max %d)", maxRuns)
	}
	out := make([]Run, 0, len(in))
	total := 0
	for _, r := range in {
		if r.T == "" {
			continue
		}
		total += utf8.RuneCountInString(r.T)
		if !profileFonts[r.Font] {
			r.Font = ""
		}
		if r.Size < 0 || r.Size > 7 {
			r.Size = 0
		}
		if !hexColor.MatchString(r.Color) {
			r.Color = ""
		} else {
			r.Color = strings.ToLower(r.Color)
		}
		if !hexColor.MatchString(r.BG) {
			r.BG = ""
		} else {
			r.BG = strings.ToLower(r.BG)
		}
		out = append(out, r)
	}
	if total > ProfileLimit {
		return nil, fmt.Errorf("profile is %d characters; the limit is %d", total, ProfileLimit)
	}
	return out, nil
}

// ---------- HTTP ----------

// Chat wires the store, the shared auth store and the hub.
type Chat struct {
	store *Store
	auth  *shared.Store
	Hub   *Hub
}

func NewChat(store *Store, auth *shared.Store) *Chat {
	return &Chat{store: store, auth: auth, Hub: NewHub(store, hxhMembers{auth})}
}

// hxhMembers adapts the shared store to the hub's member source.
type hxhMembers struct{ auth *shared.Store }

func (m hxhMembers) ListMembers() ([]shared.Member, error) { return m.auth.ListMembers(ChatWebsite) }
func (m hxhMembers) TouchLastSeen(username string)         { m.auth.TouchLastSeen(username) }

type ctxKey int

const ctxUser ctxKey = 1

// member resolves the request's shared session to a username holding a
// role on hxh.
func (c *Chat) member(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(shared.CookieName)
	if err != nil {
		return "", false
	}
	username, ok := c.auth.GetSession(cookie.Value)
	if !ok {
		return "", false
	}
	is, err := c.auth.IsMember(username, ChatWebsite)
	if err != nil {
		log.Printf("[hxh chat] member check error: %v", err)
		return "", false
	}
	return username, is
}

func (c *Chat) requireMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := c.member(r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, user)))
	})
}

func userOf(r *http.Request) string {
	u, _ := r.Context().Value(ctxUser).(string)
	return u
}

// Mount adds the request/response endpoints under the hxh prefix. The
// WebSocket endpoint is mounted separately (HandleWS) because it must
// outlive the server's per-request timeout.
func (c *Chat) Mount(r chi.Router) {
	r.Group(func(g chi.Router) {
		g.Use(c.requireMember)
		g.Get("/chat/contacts", c.handleContacts)
		g.Get("/chat/history", c.handleHistory)
		g.Get("/chat/profile/{username}", c.handleGetProfile)
		g.Put("/chat/profile", c.handlePutProfile)
	})
}

func (c *Chat) handleContacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := c.Hub.Contacts()
	if err != nil {
		log.Printf("[hxh chat] contacts error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"me": userOf(r), "contacts": contacts})
}

func (c *Chat) handleHistory(w http.ResponseWriter, r *http.Request) {
	user := userOf(r)
	room := r.URL.Query().Get("room")
	if !canUseRoom(user, room) {
		http.Error(w, "no such room", http.StatusForbidden)
		return
	}
	acct, err := c.auth.GetUser(user)
	if err != nil || acct == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var after time.Time
	if acct.ActivatedAt != nil {
		after = *acct.ActivatedAt
	}
	msgs, err := c.store.History(room, after, HistoryLimit)
	if err != nil {
		log.Printf("[hxh chat] history error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"room": room, "messages": msgs})
}

func (c *Chat) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	runs, updated, ok, err := c.store.GetProfile(username)
	if err != nil {
		log.Printf("[hxh chat] profile error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body := map[string]any{"username": username, "runs": runs}
	if ok {
		body["updated_at"] = updated
	}
	writeJSON(w, http.StatusOK, body)
}

func (c *Chat) handlePutProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Runs json.RawMessage `json:"runs"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	runs, err := NormalizeRuns(req.Runs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	clean, _ := json.Marshal(runs)
	if err := c.store.PutProfile(userOf(r), clean); err != nil {
		log.Printf("[hxh chat] profile save error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": userOf(r), "runs": runs})
}
