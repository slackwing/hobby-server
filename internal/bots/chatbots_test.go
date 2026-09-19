package bots

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slackwing/hobby-server/internal/shared"
)

// ---- fakes ----

type fakeStore struct {
	mu          sync.Mutex
	bots        []shared.Bot
	provisioned map[string]string
	sentence    string
}

func (s *fakeStore) ListBots(string) ([]shared.Bot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]shared.Bot, len(s.bots))
	copy(out, s.bots)
	return out, nil
}
func (s *fakeStore) SetPasswordDirect(u, h string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provisioned == nil {
		s.provisioned = map[string]string{}
	}
	s.provisioned[u] = h
	for i := range s.bots {
		if s.bots[i].Username == u {
			s.bots[i].HasPassword = true
		}
	}
	return nil
}
func (s *fakeStore) RandomSentence() (string, string, bool, error) {
	if s.sentence == "" {
		return "", "", false, nil
	}
	return s.sentence, "test", true, nil
}

type fakeConn struct {
	mu     sync.Mutex
	typing []string
	sent   []string
	closed bool
}

func (c *fakeConn) Typing(_ context.Context, room string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.typing = append(c.typing, room)
	return nil
}
func (c *fakeConn) Send(_ context.Context, room, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, room+"|"+body)
	return nil
}
func (c *fakeConn) Close() { c.mu.Lock(); c.closed = true; c.mu.Unlock() }

type fakeSite struct {
	mu       sync.Mutex
	logins   []string
	contacts []Contact
	history  map[string][]Message
	conns    []*fakeConn
	reject   bool
}

func (f *fakeSite) Login(_ context.Context, u, p string) (*Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins = append(f.logins, u+":"+p)
	if f.reject {
		return nil, ErrUnauthorized
	}
	return &Session{Cookie: "hobby_session=" + u}, nil
}
func (f *fakeSite) Contacts(context.Context, *Session) ([]Contact, error) { return f.contacts, nil }
func (f *fakeSite) History(_ context.Context, _ *Session, room string) ([]Message, error) {
	return f.history[room], nil
}
func (f *fakeSite) Connect(context.Context, *Session) (ChatConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeConn{}
	f.conns = append(f.conns, c)
	return c, nil
}

// harness: deterministic randomness and instant sleeps that are recorded
type harness struct {
	c      *ChatBots
	store  *fakeStore
	site   *fakeSite
	mu     sync.Mutex
	rolls  []float64
	sleeps []time.Duration
	now    time.Time
}

func newHarness(bots ...shared.Bot) *harness {
	h := &harness{store: &fakeStore{bots: bots, sentence: "The Owl and the Pussy-cat went to sea."}, site: &fakeSite{history: map[string][]Message{}},
		now: time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)}
	cfg := DefaultChatConfig()
	cfg.Password = "beetle-pass"
	h.c = NewChatBots(h.store, h.site, cfg)
	h.c.now = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
	h.c.rnd = func() float64 {
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.rolls) == 0 {
			return 0.5
		}
		r := h.rolls[0]
		h.rolls = h.rolls[1:]
		return r
	}
	h.c.sleep = func(_ context.Context, d time.Duration) bool {
		h.mu.Lock()
		h.sleeps = append(h.sleeps, d)
		h.mu.Unlock()
		return true
	}
	h.c.logf = func(string, ...any) {}
	return h
}

func (h *harness) roll(v ...float64) { h.mu.Lock(); h.rolls = append(h.rolls, v...); h.mu.Unlock() }

// settle waits for background speech acts to finish: quiet for 150 ms
// straight (an act may take a moment to start, and argon2 hashing in a
// re-provision takes longer than that).
func (h *harness) settle() {
	deadline := time.Now().Add(3 * time.Second)
	quietSince := time.Now()
	for time.Now().Before(deadline) {
		h.c.mu.Lock()
		busy := false
		for _, st := range h.c.bots {
			busy = busy || st.pending
		}
		h.c.mu.Unlock()
		if busy {
			quietSince = time.Now()
		} else if time.Since(quietSince) > 150*time.Millisecond {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func bot(u string, t float64, pw bool) shared.Bot {
	return shared.Bot{Username: u, DisplayName: strings.ToUpper(u[:1]) + u[1:], Talkativity: t, HasPassword: pw}
}

// ---- tests ----

func TestMaths(t *testing.T) {
	h := newHarness()
	c := h.c
	if c.weight(0) != 0.3 || c.weight(1) != 1 {
		t.Errorf("weights %v %v", c.weight(0), c.weight(1))
	}
	if c.replyChance(0) != 0.5 || c.replyChance(1) != 0.9 {
		t.Errorf("reply chances %v %v", c.replyChance(0), c.replyChance(1))
	}
	h.roll(0)
	if d := c.delay(); d != 5*time.Second {
		t.Errorf("u=0 → 5 s, got %v", d)
	}
	h.roll(1)
	if d := c.delay(); d != 60*time.Second {
		t.Errorf("u=1 → 60 s, got %v", d)
	}
	h.roll(0.5)
	if d := c.delay(); d != 5*time.Second+13750*time.Millisecond {
		t.Errorf("u=0.5 → 18.75 s (quadratic: most mass near 5 s), got %v", d)
	}
	// silence: deterministic, about a fifth of everything
	n := 0
	for i := 0; i < 2000; i++ {
		if Silence(fmt.Sprintf("line %d", i), 10, 2) {
			n++
		}
	}
	if n < 320 || n > 480 {
		t.Errorf("silence rate %d/2000, want ≈ 20%%", n)
	}
	if Silence("x", 10, 2) != Silence("x", 10, 2) {
		t.Error("silence must be deterministic")
	}
}

func TestTickRollsEveryBotByWeight(t *testing.T) {
	h := newHarness(bot("quiet", 0, true), bot("mid", 0.5, true), bot("loud", 1, true))
	// weights .3 .65 1 → sum 1.95 → chances .154 .333 .513
	h.roll(0.2, 0.2, 0.2) // quiet no, mid yes, loud yes
	h.c.Tick(context.Background())
	h.settle()
	sent := 0
	for _, c := range h.site.conns {
		sent += len(c.sent)
	}
	if sent != 2 {
		t.Fatalf("mid and loud should speak, got %d sends", sent)
	}
	if len(h.site.logins) != 2 || !strings.HasSuffix(h.site.logins[0], ":beetle-pass") {
		t.Fatalf("each speaker logs in with the bot password: %v", h.site.logins)
	}
}

func TestSpeakFlow(t *testing.T) {
	h := newHarness(bot("alyosha", 0.5, true))
	h.site.contacts = []Contact{{Username: "alyosha"}, {Username: "andrew"}, {Username: "abi"}}
	// rolls: tick chance (weight .65/.65 = 1 → any roll passes), delay u, target index, linger
	h.roll(0.0, 0.5, 0.99, 0.0)
	h.c.Tick(context.Background())
	h.settle()
	if len(h.site.conns) != 1 {
		t.Fatal("one connection")
	}
	c := h.site.conns[0]
	if len(c.typing) != 5 {
		t.Fatalf("five typing signals in the last five seconds, got %d", len(c.typing))
	}
	if len(c.sent) != 1 || c.sent[0] != "global|The Owl and the Pussy-cat went to sea." {
		t.Fatalf("target index past the others → global; sent %v", c.sent)
	}
	if !c.closed {
		t.Error("socket closed after lingering")
	}
	// sleeps: delay minus the 5 s lead, then 5 × 1 s, then the linger
	want := []time.Duration{18750*time.Millisecond - 5*time.Second, time.Second, time.Second, time.Second, time.Second, time.Second, 30 * time.Second}
	if fmt.Sprint(h.sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps %v, want %v", h.sleeps, want)
	}
	if h.c.state("alyosha").lastSpoke["global"].IsZero() {
		t.Error("lastSpoke recorded")
	}
	if h.c.Acts() != 1 {
		t.Error("one act")
	}
}

func TestSpeakPicksDMUniformly(t *testing.T) {
	h := newHarness(bot("alyosha", 1, true))
	h.site.contacts = []Contact{{Username: "alyosha"}, {Username: "andrew"}, {Username: "abi"}}
	h.roll(0, 0, 0.4, 0) // target index 1 of 3 candidates (andrew, abi, global) → abi
	h.c.Tick(context.Background())
	h.settle()
	if got := h.site.conns[0].sent[0]; !strings.HasPrefix(got, "dm:abi:alyosha|") {
		t.Fatalf("want a DM with abi, got %s", got)
	}
}

func TestHookReplyAndGates(t *testing.T) {
	h := newHarness(bot("alyosha", 0, true), bot("fyodor", 1, true))
	h.c.Tick(context.Background()) // registers the bots (rolls default 0.5: chances .23/.77 → fyodor speaks)
	h.settle()
	h.site.conns = nil
	h.sleeps = nil
	// a DM to alyosha from andrew: reply chance 0.5
	msg := "Hello alyosha, are you there?"
	for Silence(msg, 10, 2) {
		msg += "!"
	}
	h.roll(0.6) // ≥ 0.5 → no reply
	h.c.Hook(context.Background(), Event{Kind: "chat.message", Room: "dm:alyosha:andrew", Sender: "andrew", Body: msg})
	h.settle()
	if len(h.site.conns) != 0 {
		t.Fatal("rolled above the reply chance: silence")
	}
	h.roll(0.4, 0, 0) // reply, delay u=0, linger
	h.c.Hook(context.Background(), Event{Kind: "chat.message", Room: "dm:alyosha:andrew", Sender: "andrew", Body: msg})
	h.settle()
	if len(h.site.conns) != 1 || !strings.HasPrefix(h.site.conns[0].sent[0], "dm:alyosha:andrew|") {
		t.Fatalf("alyosha replies in the DM, got %v", h.site.conns)
	}
	// a message that hashes to silence wakes nobody
	quiet := "x"
	for !Silence(quiet, 10, 2) {
		quiet += "x"
	}
	h.site.conns = nil
	h.roll(0, 0, 0)
	h.c.Hook(context.Background(), Event{Kind: "chat.message", Room: "dm:alyosha:andrew", Sender: "andrew", Body: quiet})
	h.settle()
	if len(h.site.conns) != 0 {
		t.Fatal("silent message must wake nobody")
	}
	// send-time re-check: the latest message in the room hashes to silence → the bot shuts up
	h.site.history["dm:alyosha:andrew"] = []Message{{Body: quiet}}
	h.roll(0, 0, 0)
	h.c.Hook(context.Background(), Event{Kind: "chat.message", Room: "dm:alyosha:andrew", Sender: "andrew", Body: msg})
	h.settle()
	if len(h.site.conns) != 1 || len(h.site.conns[0].sent) != 0 || !h.site.conns[0].closed {
		t.Fatalf("typed, then checked the latest message and shut up; got %+v", h.site.conns)
	}
	if len(h.site.conns[0].typing) != 5 {
		t.Error("it still typed for five seconds before checking")
	}
}

func TestGlobalWakesEveryoneWithCooldown(t *testing.T) {
	h := newHarness(bot("alyosha", 1, true), bot("fyodor", 1, true), bot("ivan", 1, true))
	h.roll(1, 1, 1) // nobody initiates
	h.c.Tick(context.Background())
	h.settle()
	msg := "Who is coming as Hisoka?"
	for Silence(msg, 10, 2) {
		msg += "?"
	}
	h.roll(0, 0, 0, 0, 0, 0, 0, 0, 0) // everyone replies, instantly, short linger
	h.c.Hook(context.Background(), Event{Kind: "chat.message", Room: "global", Sender: "andrew", Body: msg})
	h.settle()
	if len(h.site.conns) != 3 {
		t.Fatalf("all three bots reply to a global message, got %d", len(h.site.conns))
	}
	// a second global message within the cooldown wakes none of them
	h.site.conns = nil
	h.roll(0, 0, 0, 0, 0, 0, 0, 0, 0)
	h.c.Hook(context.Background(), Event{Kind: "chat.message", Room: "global", Sender: "andrew", Body: msg})
	h.settle()
	if len(h.site.conns) != 0 {
		t.Fatalf("cooldown: nobody speaks again in global for 10 min, got %d", len(h.site.conns))
	}
	// the sender itself is never woken; after the cooldown they answer again
	h.mu.Lock()
	h.now = h.now.Add(11 * time.Minute)
	h.mu.Unlock()
	h.roll(0, 0, 0, 0, 0, 0, 0, 0, 0)
	h.c.Hook(context.Background(), Event{Kind: "chat.message", Room: "global", Sender: "ivan", Body: msg})
	h.settle()
	if len(h.site.conns) != 2 {
		t.Fatalf("two bots (not the sender) reply after the cooldown, got %d", len(h.site.conns))
	}
}

func TestOnePendingSpeechPerBot(t *testing.T) {
	h := newHarness(bot("alyosha", 1, true))
	block := make(chan struct{})
	h.c.sleep = func(context.Context, time.Duration) bool { <-block; return true }
	h.roll(0, 0.5)
	h.c.Tick(context.Background()) // alyosha starts speaking and blocks in the delay
	time.Sleep(20 * time.Millisecond)
	if !h.c.state("alyosha").pending {
		t.Fatal("pending while waiting to speak")
	}
	h.roll(0, 0)
	h.c.Tick(context.Background()) // a second tick cannot start a second act
	h.c.Hook(context.Background(), Event{Kind: "chat.message", Room: "global", Sender: "andrew", Body: "hey!"})
	time.Sleep(20 * time.Millisecond)
	if h.c.Acts() != 1 {
		t.Fatalf("only one act while pending, got %d", h.c.Acts())
	}
	close(block)
	h.settle()
}

func TestProvisioningAndRelogin(t *testing.T) {
	h := newHarness(bot("newbie", 0.5, false))
	h.c.Tick(context.Background())
	h.settle()
	hash := h.store.provisioned["newbie"]
	if hash == "" || !shared.VerifyPassword("beetle-pass", hash) {
		t.Fatal("a bot without a password gets the configured one, argon2-hashed")
	}
	// a stale hash (password changed in config): login 401 → re-provision → retry
	h2 := newHarness(bot("alyosha", 1, true))
	h2.site.reject = true
	h2.roll(0, 0, 0.99, 0)
	h2.c.Tick(context.Background())
	h2.settle()
	if len(h2.site.logins) != 2 || h2.store.provisioned["alyosha"] == "" {
		t.Fatalf("expected login, re-provision, login again; got logins %v", h2.site.logins)
	}
}

func TestEmptyCorpusSaysNothing(t *testing.T) {
	h := newHarness(bot("alyosha", 1, true))
	h.store.sentence = ""
	h.roll(0, 0, 0.99, 0)
	h.c.Tick(context.Background())
	h.settle()
	if len(h.site.conns) != 1 || len(h.site.conns[0].sent) != 0 || !h.site.conns[0].closed {
		t.Fatal("no sentences → typed, then nothing sent, socket closed")
	}
}
