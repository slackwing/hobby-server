// hobby-server: multi-project auth + storage backend.
//
// One process, one binary, multiple project namespaces. Each project
// has its own database (with its own `user` and `session` tables), its
// own URL prefix on this server, and its own cookie scope.
//
// Mounted endpoints for each project P configured in
// ~/.config/hobby-server/config.yaml (URL paths shown relative to the
// project's URLPrefix):
//
//   POST <prefix>/login    {username, password}  → sets <name>_session cookie
//   POST <prefix>/logout                           → clears cookie + deletes session
//   GET  <prefix>/me                               → {username} when logged in, 401 otherwise
//
// Apache reverse-proxies the public URL slice to this server. For the
// "rv" project with url_prefix "/api/rv", Apache maps
// andrewcheong.com/rv/api/* → 127.0.0.1:5002/api/rv/*.
//
// Also: GET /healthz (no auth, used by Docker healthcheck).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slackwing/hobby-server/internal/auth"
	"github.com/slackwing/hobby-server/internal/config"
	"github.com/slackwing/hobby-server/internal/database"
	"github.com/slackwing/hobby-server/internal/prep"
	"github.com/slackwing/hobby-server/internal/rvedit"
)

// secureCookies is set once at startup based on cfg.Server.Env.
// Package-level because every cookie write needs it and we don't want
// to thread it through every signature.
var secureCookies bool

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", defaultConfigPath(), "path to config.yaml")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	secureCookies = cfg.Server.Env == "production"
	log.Printf("hobby-server starting on :%d (env=%s, projects=%d, secure_cookies=%v)",
		cfg.Server.Port, cfg.Server.Env, len(cfg.Projects), secureCookies)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Per-project (pool, store, cookie name). Each project's pool talks
	// to a DIFFERENT database; nothing shared at the DB layer.
	type projectState struct {
		project    config.Project
		pool       *pgxpool.Pool
		store      *auth.SessionStore
		cookieName string
	}
	states := make([]projectState, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		pool, err := database.NewPool(ctx, p.PostgresDSN())
		if err != nil {
			log.Fatalf("project %s: connect db: %v", p.Name, err)
		}
		defer pool.Close()
		states = append(states, projectState{
			project:    p,
			pool:       pool,
			store:      auth.NewSessionStore(pool),
			cookieName: p.Name + "_session",
		})
		log.Printf("project %q ready: db=%s url_prefix=%s cookie=%s_session cookie_path=%s",
			p.Name, p.Database.Name, p.URLPrefix, p.Name, p.CookiePath)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	// One sub-router per project, mounted at the configured URL prefix.
	for _, s := range states {
		s := s // capture in closure
		r.Route(s.project.URLPrefix, func(sub chi.Router) {
			sub.Post("/login", handleLogin(s.pool, s.store, s.project, s.cookieName))
			sub.Post("/logout", handleLogout(s.store, s.project, s.cookieName))
			sub.Get("/me", handleMe(s.store, s.cookieName))

			// Project-specific routes. Today only "rv" has a prep-checklist
			// feature; if a second project grows its own endpoints we'll
			// refactor this into a plugin pattern.
			if s.project.Name == "rv" {
				prepStore := prep.NewStore(s.pool)
				// Public read.
				sub.Get("/prep", prep.HandleList(prepStore))
				// Authed writes — wrap each handler with the session middleware.
				authMW := auth.Middleware(s.store, s.cookieName)
				sub.With(authMW).Post("/prep", prep.HandleCreate(prepStore))
				sub.With(authMW).Patch("/prep/{id}", prep.HandlePatch(prepStore))
				sub.With(authMW).Delete("/prep/{id}", prep.HandleDelete(prepStore))
				sub.With(authMW).Post("/prep/sections", prep.HandleCreateSection(prepStore))
				sub.With(authMW).Patch("/prep/sections/{id}", prep.HandlePatchSection(prepStore))
				sub.With(authMW).Delete("/prep/sections/{id}", prep.HandleDeleteSection(prepStore))

				// rvedit: editable overlay over the static catalog +
				// itinerary. See internal/rvedit/rvedit.go.
				rvStore := rvedit.NewStore(s.pool)
				sub.Get("/locations", rvedit.HandleListLocations(rvStore))
				sub.With(authMW).Post("/locations", rvedit.HandleCreateLocation(rvStore))
				sub.With(authMW).Patch("/locations/{id}", rvedit.HandlePatchLocation(rvStore))
				sub.With(authMW).Delete("/locations/{id}", rvedit.HandleDeleteLocation(rvStore))
				sub.Get("/itinerary", rvedit.HandleListItinerary(rvStore))
				sub.With(authMW).Post("/itinerary", rvedit.HandleUpsertItinerary(rvStore))
				sub.With(authMW).Patch("/itinerary/{id}", rvedit.HandlePatchItinerary(rvStore))
				sub.With(authMW).Delete("/itinerary/{id}", rvedit.HandleDeleteItinerary(rvStore))
				sub.Get("/note", rvedit.HandleGetNote(rvStore))
				sub.With(authMW).Put("/note", rvedit.HandlePutNote(rvStore))
				sub.Get("/checkins", rvedit.HandleListCheckins(rvStore))
				sub.With(authMW).Post("/checkins", rvedit.HandleCreateCheckin(rvStore))
			}
		})
	}

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Server.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()
	<-stop
	log.Printf("shutting down...")
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer c2()
	_ = srv.Shutdown(shutdownCtx)
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hobby-server", "config.yaml")
}

// ---------- handlers ----------

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func handleLogin(pool *pgxpool.Pool, store *auth.SessionStore, project config.Project, cookieName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Username == "" || req.Password == "" {
			http.Error(w, "username and password required", http.StatusBadRequest)
			return
		}
		hash, ok, err := auth.LookupUser(pool, req.Username)
		if err != nil {
			log.Printf("[%s] login lookup error: %v", project.Name, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Constant-time-ish: always do a bcrypt comparison to avoid leaking
		// user existence via timing.
		if !ok {
			auth.VerifyPassword(req.Password, dummyHash)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if !auth.VerifyPassword(req.Password, hash) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		token, err := store.Create(req.Username)
		if err != nil {
			log.Printf("[%s] session create error: %v", project.Name, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, token, project, cookieName, false)
		writeJSON(w, http.StatusOK, map[string]string{"username": req.Username})
	}
}

func handleLogout(store *auth.SessionStore, project config.Project, cookieName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(cookieName); err == nil {
			store.Delete(cookie.Value)
		}
		setSessionCookie(w, "", project, cookieName, true) // clear
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleMe(store *auth.SessionStore, cookieName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		s, ok := store.Get(cookie.Value)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"username": s.Username})
	}
}

// setSessionCookie sets the project's session cookie. Pass clear=true to
// expire it immediately.
func setSessionCookie(w http.ResponseWriter, token string, project config.Project, cookieName string, clear bool) {
	expires := time.Now().Add(auth.SessionTTL)
	maxAge := int(auth.SessionTTL.Seconds())
	if clear {
		expires = time.Unix(0, 0)
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     project.CookiePath,
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// dummyHash is a precomputed bcrypt hash used only to keep failed-login
// timing roughly constant whether or not the username exists.
const dummyHash = "$2a$10$abcdefghijklmnopqrstuuQHHpJUcD3JE9DqJxqkpAjW3kjjVeIQK"
