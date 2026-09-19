package bots

// hxh_chatbots — Andrew's spec (2026-09-19):
//
//   - Every tick (5 min) EVERY bot rolls independently. Its weight is
//     0.3 + 0.7·talkativity (metadata.talkativity, 0..1: a min and a
//     max, never never/always); its chance is weight / Σweights, so
//     about one bot starts a conversation per tick — fat tail included.
//   - Every "speak" waits 5 s + 55 s·u² first (density highest at 5 s,
//     thinning toward a minute). For the last 5 s the bot sends a typing
//     signal every second, so a real player sees "<bot> is typing…".
//   - The target is uniform over the global room and every other
//     member — real players and bots alike.
//   - A message that reaches a bot wakes it (DMs: the recipient; global:
//     everyone). It replies with chance 0.5 + 0.4·talkativity (clamped
//     50–90 %), after the same delay — unless the message hashes to
//     silence: md5(body) mod 10 < 2, "something no one wants to answer".
//     Just before sending, the bot checks the room's LATEST message the
//     same way, so when a fellow bot's reply hashes to silence, everyone
//     who was about to speak shuts up at once.
//   - Safety valve (mine): a bot that spoke in a room ignores wake-ups
//     from that room for a cooldown (10 min for global, none for DMs),
//     and holds at most one pending speech at a time — otherwise thirteen
//     bots at 50–90 % would never let the global room go quiet.
//   - After sending, the bot keeps its socket open for 30–120 s (it
//     shows online), then hangs up: online for a minute, away for an
//     hour, offline — presence you can watch change.
//
// Everything goes through the public site API like a real player; the
// only bot-specific act is provisioning the shared bot password onto
// accounts that have none yet.

import (
	"context"
	"crypto/md5"
	"log"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/slackwing/hobby-server/internal/hxh"
	"github.com/slackwing/hobby-server/internal/shared"
)

const ChatProgram = "hxh_chatbots"

// BotStore is what the program needs from the shared store.
type BotStore interface {
	ListBots(website string) ([]shared.Bot, error)
	SetPasswordDirect(username, hash string) error
	RandomSentence() (sentence, source string, ok bool, err error)
}

// Chat is the site API a bot uses (Site implements it).
type Chat interface {
	Login(ctx context.Context, username, password string) (*Session, error)
	Contacts(ctx context.Context, sess *Session) ([]Contact, error)
	History(ctx context.Context, sess *Session, room string) ([]Message, error)
	Connect(ctx context.Context, sess *Session) (ChatConn, error)
}

type ChatConn interface {
	Typing(ctx context.Context, room string) error
	Send(ctx context.Context, room, body string) error
	Close()
}

type ChatConfig struct {
	Website         string
	Password        string
	ExpectedPerTick float64       // initiations per tick across all bots (1)
	WeightMin       float64       // talkativity 0 speaks this fraction as often as 1 (0.3)
	ReplyMin        float64       // reply chance at talkativity 0 (0.5)
	ReplyMax        float64       // reply chance at talkativity 1 (0.9)
	DelayMin        time.Duration // 5 s
	DelayMax        time.Duration // 60 s
	TypingLead      time.Duration // typing signals for the last 5 s before sending
	SilenceMod      int           // md5(body) mod 10 …
	SilenceBelow    int           // … < 2 = nobody answers
	GlobalCooldown  time.Duration // a bot stays quiet in global this long after speaking there (10 min)
	LingerMin       time.Duration // hold the socket after speaking: 30 s …
	LingerMax       time.Duration // … to 120 s
}

func DefaultChatConfig() ChatConfig {
	return ChatConfig{
		Website: "hxh", ExpectedPerTick: 1, WeightMin: 0.3, ReplyMin: 0.5, ReplyMax: 0.9,
		DelayMin: 5 * time.Second, DelayMax: 60 * time.Second, TypingLead: 5 * time.Second,
		SilenceMod: 10, SilenceBelow: 2, GlobalCooldown: 10 * time.Minute,
		LingerMin: 30 * time.Second, LingerMax: 120 * time.Second,
	}
}

type botState struct {
	bot       shared.Bot
	sess      *Session
	pending   bool
	lastSpoke map[string]time.Time
}

type ChatBots struct {
	cfg   ChatConfig
	store BotStore
	site  Chat
	now   func() time.Time
	rnd   func() float64
	sleep func(ctx context.Context, d time.Duration) bool
	logf  func(format string, args ...any)

	mu   sync.Mutex
	bots map[string]*botState
	acts int // speech acts started (for the console)
}

func NewChatBots(store BotStore, site Chat, cfg ChatConfig) *ChatBots {
	src := rand.New(rand.NewSource(time.Now().UnixNano()))
	return &ChatBots{
		cfg: cfg, store: store, site: site,
		now:   func() time.Time { return time.Now().UTC() },
		rnd:   src.Float64,
		sleep: realSleep,
		logf:  log.Printf,
		bots:  map[string]*botState{},
	}
}

func realSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *ChatBots) Name() string { return ChatProgram }

// ---------- the maths ----------

func (c *ChatBots) weight(t float64) float64 { return c.cfg.WeightMin + (1-c.cfg.WeightMin)*t }

func (c *ChatBots) replyChance(t float64) float64 {
	return c.cfg.ReplyMin + (c.cfg.ReplyMax-c.cfg.ReplyMin)*t
}

// delay: 5 s + 55 s·u² — most likely near 5 s, rarely near a minute.
func (c *ChatBots) delay() time.Duration {
	u := c.rnd()
	return c.cfg.DelayMin + time.Duration(float64(c.cfg.DelayMax-c.cfg.DelayMin)*u*u)
}

func (c *ChatBots) linger() time.Duration {
	return c.cfg.LingerMin + time.Duration(float64(c.cfg.LingerMax-c.cfg.LingerMin)*c.rnd())
}

// Silence: md5(body) as a number, mod 10 < 2 → nobody answers this one.
func Silence(body string, mod, below int) bool {
	sum := md5.Sum([]byte(body))
	n := new(big.Int).SetBytes(sum[:])
	return int(new(big.Int).Mod(n, big.NewInt(int64(mod))).Int64()) < below
}

func (c *ChatBots) silent(body string) bool {
	return Silence(body, c.cfg.SilenceMod, c.cfg.SilenceBelow)
}

// ---------- state ----------

func (c *ChatBots) refresh(ctx context.Context) []*botState {
	bots, err := c.store.ListBots(c.cfg.Website)
	if err != nil {
		c.logf("[bots] list error: %v", err)
		return nil
	}
	var hash string
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*botState, 0, len(bots))
	for _, b := range bots {
		st := c.bots[b.Username]
		if st == nil {
			st = &botState{lastSpoke: map[string]time.Time{}}
			c.bots[b.Username] = st
		}
		st.bot = b
		if !b.HasPassword && c.cfg.Password != "" {
			if hash == "" {
				if h, err := shared.HashPassword(c.cfg.Password); err == nil {
					hash = h
				}
			}
			if hash != "" {
				if err := c.store.SetPasswordDirect(b.Username, hash); err != nil {
					c.logf("[bots] provision %s: %v", b.Username, err)
				} else {
					st.bot.HasPassword = true
					c.logf("[bots] provisioned password for %s", b.Username)
				}
			}
		}
		out = append(out, st)
	}
	return out
}

func (c *ChatBots) state(user string) *botState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bots[user]
}

// Tick: every bot rolls; the lucky ones start a conversation.
func (c *ChatBots) Tick(ctx context.Context) {
	states := c.refresh(ctx)
	var sum float64
	for _, st := range states {
		sum += c.weight(st.bot.Talkativity)
	}
	if sum == 0 {
		return
	}
	for _, st := range states {
		p := c.cfg.ExpectedPerTick * c.weight(st.bot.Talkativity) / sum
		c.mu.Lock()
		busy := st.pending || !st.bot.HasPassword
		c.mu.Unlock()
		if busy || c.rnd() >= p {
			continue
		}
		go c.speak(ctx, st, "", nil)
	}
}

// Hook: a stored chat message wakes the bots it reached.
func (c *ChatBots) Hook(ctx context.Context, ev Event) {
	if ev.Kind != "chat.message" {
		return
	}
	if c.silent(ev.Body) {
		return
	}
	for _, st := range c.wokenBy(ev) {
		c.mu.Lock()
		skip := st.pending || !st.bot.HasPassword
		if ev.Room == hxh.RoomGlobal && c.now().Sub(st.lastSpoke[ev.Room]) < c.cfg.GlobalCooldown {
			skip = true
		}
		c.mu.Unlock()
		if skip || c.rnd() >= c.replyChance(st.bot.Talkativity) {
			continue
		}
		e := ev
		go c.speak(ctx, st, ev.Room, &e)
	}
}

// wokenBy: everyone but the sender for global; the other party for a DM.
func (c *ChatBots) wokenBy(ev Event) []*botState {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*botState
	if ev.Room == hxh.RoomGlobal {
		for u, st := range c.bots {
			if u != ev.Sender {
				out = append(out, st)
			}
		}
		return out
	}
	for u, st := range c.bots {
		if u != ev.Sender && hxh.DMRoom(u, ev.Sender) == ev.Room {
			out = append(out, st)
		}
	}
	return out
}

func (c *ChatBots) session(ctx context.Context, st *botState) (*Session, error) {
	c.mu.Lock()
	sess, user := st.sess, st.bot.Username
	c.mu.Unlock()
	if sess != nil {
		return sess, nil
	}
	sess, err := c.site.Login(ctx, user, c.cfg.Password)
	if err == ErrUnauthorized && c.cfg.Password != "" {
		// the configured password changed under a provisioned account: re-provision once
		if h, herr := shared.HashPassword(c.cfg.Password); herr == nil && c.store.SetPasswordDirect(user, h) == nil {
			sess, err = c.site.Login(ctx, user, c.cfg.Password)
		}
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	st.sess = sess
	c.mu.Unlock()
	return sess, nil
}

func (c *ChatBots) forget(st *botState) {
	c.mu.Lock()
	st.sess = nil
	c.mu.Unlock()
}

// speak is one speech act: wait, pick a room if none, log in, type for
// the last five seconds, check the silence gate for replies, send, then
// hang around a while before hanging up.
func (c *ChatBots) speak(ctx context.Context, st *botState, room string, replyTo *Event) {
	c.mu.Lock()
	if st.pending {
		c.mu.Unlock()
		return
	}
	st.pending = true
	c.acts++
	user := st.bot.Username
	c.mu.Unlock()
	done := false
	release := func() {
		if !done {
			done = true
			c.mu.Lock()
			st.pending = false
			c.mu.Unlock()
		}
	}
	defer release()

	d := c.delay()
	lead := c.cfg.TypingLead
	if d > lead {
		if !c.sleep(ctx, d-lead) {
			return
		}
		d = lead
	}
	sess, err := c.session(ctx, st)
	if err != nil {
		c.logf("[bots] %s login: %v", user, err)
		return
	}
	if room == "" {
		contacts, err := c.site.Contacts(ctx, sess)
		if err != nil {
			c.logf("[bots] %s contacts: %v", user, err)
			c.forget(st)
			return
		}
		room = c.pickRoom(user, contacts)
	}
	conn, err := c.site.Connect(ctx, sess)
	if err != nil {
		c.logf("[bots] %s connect: %v", user, err)
		c.forget(st)
		return
	}
	// "is typing…" once a second for the lead-in
	for left := d; left > 0; left -= time.Second {
		_ = conn.Typing(ctx, room)
		if !c.sleep(ctx, minDur(time.Second, left)) {
			conn.Close()
			return
		}
	}
	if replyTo != nil {
		hist, err := c.site.History(ctx, sess, room)
		if err == nil && len(hist) > 0 && c.silent(hist[len(hist)-1].Body) {
			c.logf("[bots] %s shuts up in %s", user, room)
			conn.Close()
			return
		}
	}
	sentence, _, ok, err := c.store.RandomSentence()
	if err != nil || !ok {
		c.logf("[bots] %s has nothing to say (corpus empty: %v)", user, err)
		conn.Close()
		return
	}
	if err := conn.Send(ctx, room, sentence); err != nil {
		c.logf("[bots] %s send: %v", user, err)
		conn.Close()
		c.forget(st)
		return
	}
	c.logf("[bots] %s → %s: %q", user, room, sentence)
	c.mu.Lock()
	st.lastSpoke[room] = c.now()
	c.mu.Unlock()
	release()
	c.sleep(ctx, c.linger())
	conn.Close()
}

// pickRoom: the global room or a DM with any other member, uniformly.
func (c *ChatBots) pickRoom(user string, contacts []Contact) string {
	others := make([]string, 0, len(contacts))
	for _, ct := range contacts {
		if ct.Username != user {
			others = append(others, ct.Username)
		}
	}
	i := int(c.rnd() * float64(len(others)+1))
	if i >= len(others) {
		return hxh.RoomGlobal
	}
	return hxh.DMRoom(user, others[i])
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// Acts reports how many speech acts have started (console status).
func (c *ChatBots) Acts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acts
}
