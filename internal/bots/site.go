package bots

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

// Site is the public web API as a bot uses it — nothing the browser
// doesn't also do. Direct=true targets the Go server itself (paths
// /api/<site>/…) instead of the public site (paths /<site>/api/…).
type Site struct {
	Base   string
	Direct bool
	HTTP   *http.Client
}

// Session is a logged-in bot: its hobby_session cookie.
type Session struct {
	Cookie string
}

type Contact struct {
	Username string `json:"username"`
	State    string `json:"state"`
	IsBot    bool   `json:"is_bot"`
}

type Message struct {
	ID        int64     `json:"id"`
	Room      string    `json:"room"`
	Sender    string    `json:"sender"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

var ErrUnauthorized = errors.New("unauthorized")

func (s *Site) path(site, rest string) string {
	if s.Direct {
		return s.Base + "/api/" + site + rest
	}
	return s.Base + "/" + site + "/api" + rest
}

func (s *Site) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (s *Site) do(ctx context.Context, sess *Session, method, u string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sess != nil {
		req.Header.Set("Cookie", sess.Cookie)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s %s: %d %s", method, u, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Login is POST /admin/api/login — the session cookie comes back.
func (s *Site) Login(ctx context.Context, username, password string) (*Session, error) {
	b, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := http.NewRequestWithContext(ctx, "POST", s.path("admin", "/login"), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("login: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "hobby_session" {
			return &Session{Cookie: c.Name + "=" + c.Value}, nil
		}
	}
	return nil, errors.New("login: no session cookie")
}

func (s *Site) Contacts(ctx context.Context, sess *Session) ([]Contact, error) {
	var out struct {
		Contacts []Contact `json:"contacts"`
	}
	err := s.do(ctx, sess, "GET", s.path("hxh", "/chat/contacts"), nil, &out)
	return out.Contacts, err
}

func (s *Site) History(ctx context.Context, sess *Session, room string) ([]Message, error) {
	var out struct {
		Messages []Message `json:"messages"`
	}
	err := s.do(ctx, sess, "GET", s.path("hxh", "/chat/history?room="+url.QueryEscape(room)), nil, &out)
	return out.Messages, err
}

// Conn is an open chat socket.
type Conn struct {
	ws *websocket.Conn
}

func (s *Site) Connect(ctx context.Context, sess *Session) (ChatConn, error) {
	u := s.path("hxh", "/chat/ws")
	if len(u) > 4 && u[:4] == "http" {
		u = "ws" + u[4:]
	}
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {sess.Cookie}}, HTTPClient: s.client()})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	ws.SetReadLimit(1 << 20)
	c := &Conn{ws: ws}
	go func() { // drain: the hub's frames to this bot are not needed here
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	return c, nil
}

func (c *Conn) send(ctx context.Context, v any) error {
	b, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, b)
}

func (c *Conn) Typing(ctx context.Context, room string) error {
	return c.send(ctx, map[string]any{"t": "typing", "room": room})
}

func (c *Conn) Send(ctx context.Context, room, body string) error {
	return c.send(ctx, map[string]any{"t": "msg", "room": room, "body": body})
}

func (c *Conn) Close() { _ = c.ws.Close(websocket.StatusNormalClosure, "bye") }
