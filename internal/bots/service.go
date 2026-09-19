// Package bots runs "bot programs": fake users that use the PUBLIC site
// API exactly like real players (log in, open the chat socket, type,
// send), so they double as end-to-end testers. Each program is a row in
// shared_bot_program and can be switched on and off from the console;
// the service ticks every program on a timer and forwards hooks (e.g. a
// chat message reaching a bot) to the enabled ones.
package bots

import (
	"context"
	"log"
	"sync"
	"time"
)

// Event is something the server tells the programs about.
type Event struct {
	Kind   string // "chat.message"
	Room   string
	Sender string
	Body   string
	ID     int64
}

// Program is one bot behaviour, e.g. the hxh chat bots.
type Program interface {
	Name() string
	// Tick runs once per scheduler interval (in its own goroutine).
	Tick(ctx context.Context)
	// Hook receives server events (in its own goroutine).
	Hook(ctx context.Context, ev Event)
}

// Registry says which programs are switched on (shared_bot_program).
type Registry interface {
	BotProgramEnabled(name string) (bool, error)
}

type Status struct {
	Name     string    `json:"name"`
	Enabled  bool      `json:"enabled"`
	LastTick time.Time `json:"last_tick"`
	Ticks    int       `json:"ticks"`
	Hooks    int       `json:"hooks"`
}

type Service struct {
	reg      Registry
	tick     time.Duration
	programs []Program
	mu       sync.Mutex
	status   map[string]*Status
	logf     func(format string, args ...any)
}

func New(reg Registry, tick time.Duration) *Service {
	return &Service{reg: reg, tick: tick, status: map[string]*Status{}, logf: log.Printf}
}

func (s *Service) Add(p Program) {
	s.programs = append(s.programs, p)
	s.status[p.Name()] = &Status{Name: p.Name()}
}

func (s *Service) enabled(name string) bool {
	on, err := s.reg.BotProgramEnabled(name)
	if err != nil {
		s.logf("[bots] registry error for %s: %v", name, err)
		return false
	}
	return on
}

// Run ticks every enabled program until ctx ends. The first tick is
// immediate so a fresh deploy shows life without waiting an interval.
func (s *Service) Run(ctx context.Context) {
	s.TickAll(ctx)
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.TickAll(ctx)
		}
	}
}

func (s *Service) TickAll(ctx context.Context) {
	for _, p := range s.programs {
		if !s.enabled(p.Name()) {
			continue
		}
		s.mu.Lock()
		st := s.status[p.Name()]
		st.LastTick = time.Now()
		st.Ticks++
		s.mu.Unlock()
		go p.Tick(ctx)
	}
}

// Hook forwards an event to every enabled program.
func (s *Service) Hook(ctx context.Context, ev Event) {
	for _, p := range s.programs {
		if !s.enabled(p.Name()) {
			continue
		}
		s.mu.Lock()
		s.status[p.Name()].Hooks++
		s.mu.Unlock()
		go p.Hook(ctx, ev)
	}
}

func (s *Service) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.programs))
	for _, p := range s.programs {
		st := *s.status[p.Name()]
		st.Enabled = s.enabled(p.Name())
		out = append(out, st)
	}
	return out
}
