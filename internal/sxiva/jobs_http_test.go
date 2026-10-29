package sxiva

// Who may use the roles endpoints: reads for the global admin and anyone
// holding a role on the status site (tampopo views read-only); writes
// for the global admin only. A fake auth store; the roles come from the
// SXIVA_TEST_DB_URL database (skips without it).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/slackwing/hobby-server/internal/shared"
)

type fakeJobsAuth struct {
	sessions map[string]string // cookie → username
	roles    []string          // "user website role"
}

func (f fakeJobsAuth) GetSession(token string) (string, bool) {
	u, ok := f.sessions[token]
	return u, ok
}

func (f fakeJobsAuth) HasRole(user, website, role string) (bool, error) {
	for _, r := range f.roles {
		if r == user+" "+website+" "+role {
			return true, nil
		}
	}
	return false, nil
}

func (f fakeJobsAuth) MayAct(user, website string) (bool, error) {
	for _, r := range f.roles {
		if strings.HasPrefix(r, user+" "+website+" ") && !strings.HasSuffix(r, " anonymous") {
			return true, nil
		}
	}
	return false, nil
}

func TestJobsAccess(t *testing.T) {
	store := jobsTestStore(t)
	title := "Software Engineer 5 - Access Check"
	if _, err := store.SaveListings(SourceNetflix, []Listing{{Source: SourceNetflix, ExternalID: "1", Title: title, PostedAt: time.Now().UTC()}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	role := RoleID(title)
	auth := fakeJobsAuth{
		sessions: map[string]string{"a": "andrew", "t": "tampopo", "b": "bapper", "n": "anonymous"},
		roles: []string{
			"andrew admin admin", "andrew status admin",
			"tampopo status viewer", "tampopo bap player",
			"bapper bap player",
			"anonymous status anonymous",
		},
	}
	w := NewJobWatcher(store)
	w.netflixBase, w.linkedInBase = "http://127.0.0.1:1", "http://127.0.0.1:1" // a refresh must never reach the real sites
	r := chi.NewRouter()
	mountJobs(r, store, &Jobs{Watcher: w, Admin: auth})

	call := func(cookie, method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: shared.CookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	cases := []struct {
		who, cookie, method, path, body string
		want                            int
	}{
		{"nobody", "", "GET", "/jobs", "", 401},
		{"a stale cookie", "zz", "GET", "/jobs", "", 401},
		{"tampopo", "t", "GET", "/jobs", "", 200},
		{"tampopo", "t", "GET", "/jobs/roles/" + role, "", 200},
		{"tampopo", "t", "PATCH", "/jobs/roles/" + role, `{"favorite": true}`, 403},
		{"tampopo", "t", "PATCH", "/jobs/roles/" + role, `{"seen": true}`, 403},
		{"tampopo", "t", "POST", "/jobs/refresh", "", 403},
		{"a bap-only player", "b", "GET", "/jobs", "", 403},
		{"the anonymous account", "n", "GET", "/jobs", "", 403},
		{"andrew", "a", "GET", "/jobs", "", 200},
		{"andrew", "a", "PATCH", "/jobs/roles/" + role, `{"hidden": true}`, 204},
	}
	for _, c := range cases {
		if got := call(c.cookie, c.method, c.path, c.body); got != c.want {
			t.Errorf("%s: %s %s → %d, want %d", c.who, c.method, c.path, got, c.want)
		}
	}
	roles, _, err := store.JobsView()
	if err != nil || len(roles) != 1 || roles[0].Favorite || roles[0].SeenAt != nil || !roles[0].Hidden {
		t.Errorf("only andrew's write may land: %+v %v", roles, err)
	}
}
