package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"fanoutd/internal/agent"
	"fanoutd/internal/config"
	"fanoutd/internal/store"
)

// The token is the only thing between a network and someone else's board, so
// the gate is tested through the real routing table rather than by calling the
// handlers directly: a handler that refuses correctly behind a route that never
// reaches it is not a gate.

const testToken = "s3cret-token"

func authServer(t *testing.T, token string) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	loop := agent.NewLoop(s, nil, filepath.Join(dir, "output"))
	ui := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html>")}}
	return New(s, loop, nil, config.Config{Token: token}, ui)
}

// send runs a request through the gated routing table.
func send(t *testing.T, s *Server, r *http.Request) *http.Response {
	t.Helper()
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w.Result()
}

func login(t *testing.T, s *Server, token string) *http.Response {
	t.Helper()
	body := strings.NewReader(`{"token":` + strconvQuote(token) + `}`)
	return send(t, s, httptest.NewRequest("POST", "/api/auth/login", body))
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// sessionCookieOf returns the session cookie the response sets, or nil.
func sessionCookieOf(res *http.Response) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func TestLoginIssuesASessionForTheRightToken(t *testing.T) {
	s := authServer(t, testToken)
	res := login(t, s, testToken)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	c := sessionCookieOf(res)
	if c == nil {
		t.Fatal("login set no session cookie")
	}
	if c.Value != testToken {
		t.Errorf("cookie value = %q, want the token", c.Value)
	}
	if !c.HttpOnly {
		t.Error("session cookie is readable from JavaScript")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.MaxAge != cookieMaxAge {
		t.Errorf("MaxAge = %d, want %d", c.MaxAge, cookieMaxAge)
	}
}

func TestLoginRejectsAWrongToken(t *testing.T) {
	s := authServer(t, testToken)
	res := login(t, s, "not-the-token")

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if c := sessionCookieOf(res); c != nil {
		t.Errorf("a refused login still set a session cookie: %q", c.Value)
	}
}

// An empty candidate must not match an empty configured token by way of the
// login route — auth being off is handled before the comparison, not by it.
func TestLoginRefusesWhenAuthIsDisabled(t *testing.T) {
	s := authServer(t, "")
	res := login(t, s, "")

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if c := sessionCookieOf(res); c != nil {
		t.Error("a server with no token handed out a session")
	}
}

// A token pasted from a password manager arrives with a newline on it.
func TestLoginTrimsSurroundingWhitespace(t *testing.T) {
	s := authServer(t, testToken)
	res := login(t, s, "  "+testToken+"\n")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}

func TestLoginRejectsAMalformedBody(t *testing.T) {
	s := authServer(t, testToken)
	res := send(t, s, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader("not json")))

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if c := sessionCookieOf(res); c != nil {
		t.Error("a malformed login set a session cookie")
	}
}

// The login route is reachable without a token, so it must not answer a GET —
// a token in a query string lands in logs and browser history.
func TestLoginRefusesAGet(t *testing.T) {
	s := authServer(t, testToken)
	res := send(t, s, httptest.NewRequest("GET", "/api/auth/login?token="+testToken, nil))

	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
	if c := sessionCookieOf(res); c != nil {
		t.Error("a GET login set a session cookie")
	}
}

func TestSessionCookieIsSecureBehindATLSProxy(t *testing.T) {
	s := authServer(t, testToken)
	r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"token":"`+testToken+`"}`))
	r.Header.Set("X-Forwarded-Proto", "https")

	c := sessionCookieOf(send(t, s, r))
	if c == nil {
		t.Fatal("login set no session cookie")
	}
	if !c.Secure {
		t.Error("session cookie is not Secure behind a TLS-terminating proxy")
	}
}

// On plain http a Secure cookie would never be sent back, locking the user out
// of their own homelab board.
func TestSessionCookieIsNotSecureOnPlainHTTP(t *testing.T) {
	s := authServer(t, testToken)
	c := sessionCookieOf(login(t, s, testToken))
	if c == nil {
		t.Fatal("login set no session cookie")
	}
	if c.Secure {
		t.Error("session cookie is Secure over plain http and will never come back")
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	s := authServer(t, testToken)
	r := httptest.NewRequest("POST", "/api/auth/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: testToken})
	res := send(t, s, r)

	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.StatusCode)
	}
	c := sessionCookieOf(res)
	if c == nil {
		t.Fatal("logout set no session cookie")
	}
	if c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("logout left the cookie alive: value %q, MaxAge %d", c.Value, c.MaxAge)
	}
}

// Neither status endpoint changes state, so neither answers a method that
// implies it did.
func TestAuthEndpointsRefuseTheWrongMethod(t *testing.T) {
	tests := []struct{ method, path string }{
		{"POST", "/api/auth"},
		{"GET", "/api/auth/logout"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			s := authServer(t, testToken)
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r.Header.Set("Authorization", "Bearer "+testToken)

			if res := send(t, s, r); res.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", res.StatusCode)
			}
		})
	}
}

func TestAuthStatusReportsWhetherATokenIsNeeded(t *testing.T) {
	tests := []struct {
		name          string
		token         string
		send          string
		required      bool
		authenticated bool
	}{
		{"no token configured", "", "", false, true},
		{"token needed, none sent", testToken, "", true, false},
		{"token needed, wrong one sent", testToken, "wrong", true, false},
		{"token needed and held", testToken, testToken, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := authServer(t, tt.token)
			r := httptest.NewRequest("GET", "/api/auth", nil)
			if tt.send != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tt.send})
			}

			var got map[string]bool
			if err := json.NewDecoder(send(t, s, r).Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got["required"] != tt.required {
				t.Errorf("required = %v, want %v", got["required"], tt.required)
			}
			if got["authenticated"] != tt.authenticated {
				t.Errorf("authenticated = %v, want %v", got["authenticated"], tt.authenticated)
			}
		})
	}
}

// Only login is public under /api/auth/; anything else invented there is gated
// like the rest of the API, so an unauthenticated caller cannot even learn
// which actions exist.
func TestAnUnknownAuthActionIsGated(t *testing.T) {
	s := authServer(t, testToken)
	res := send(t, s, httptest.NewRequest("POST", "/api/auth/elevate", nil))

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

func TestAnUnknownAuthActionIsRejectedOncePastTheGate(t *testing.T) {
	s := authServer(t, testToken)
	r := httptest.NewRequest("POST", "/api/auth/elevate", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)

	if res := send(t, s, r); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// Every way in has to be gated, not just the read paths.
func TestGatedRoutesRefuseAnUnauthenticatedCaller(t *testing.T) {
	s := authServer(t, testToken)
	requests := []struct{ method, path string }{
		{"GET", "/api/tasks"},
		{"POST", "/api/tasks"},
		{"GET", "/api/tasks/some-id"},
		{"DELETE", "/api/tasks/some-id"},
		{"POST", "/api/tasks/some-id/start"},
		{"POST", "/api/breakdown"},
		{"GET", "/api/groups/some-id/plan"},
		{"GET", "/api/config"},
		{"GET", "/api/models"},
		{"GET", previewPrefix + "ws/index.html"},
	}
	for _, req := range requests {
		t.Run(req.method+" "+req.path, func(t *testing.T) {
			res := send(t, s, httptest.NewRequest(req.method, req.path, nil))
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", res.StatusCode)
			}
		})
	}
}

// The CLI sends the same secret as a bearer header rather than a cookie.
func TestBearerTokenAuthorizesTheCLI(t *testing.T) {
	s := authServer(t, testToken)
	r := httptest.NewRequest("GET", "/api/tasks", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)

	if res := send(t, s, r); res.StatusCode == http.StatusUnauthorized {
		t.Error("a correct bearer token was refused")
	}
}

func TestABadBearerTokenIsRefused(t *testing.T) {
	s := authServer(t, testToken)
	for _, header := range []string{"Bearer wrong", "Basic " + testToken, testToken, "Bearer "} {
		t.Run(header, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/tasks", nil)
			r.Header.Set("Authorization", header)
			if res := send(t, s, r); res.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", res.StatusCode)
			}
		})
	}
}

// A session obtained by logging in has to actually open the board, or the whole
// exchange is theatre.
func TestASessionFromLoginOpensTheBoard(t *testing.T) {
	s := authServer(t, testToken)
	c := sessionCookieOf(login(t, s, testToken))
	if c == nil {
		t.Fatal("login set no session cookie")
	}

	r := httptest.NewRequest("GET", "/api/tasks", nil)
	r.AddCookie(c)
	if res := send(t, s, r); res.StatusCode == http.StatusUnauthorized {
		t.Error("the cookie login just issued does not authorize")
	}
}

// The health check stays reachable so a monitor can poll it, and the UI's own
// files stay open so the login page can load before a token exists.
func TestPublicPathsStayReachable(t *testing.T) {
	s := authServer(t, testToken)
	for _, path := range []string{"/api/health", "/api/auth", "/", "/index.html"} {
		t.Run(path, func(t *testing.T) {
			if res := send(t, s, httptest.NewRequest("GET", path, nil)); res.StatusCode == http.StatusUnauthorized {
				t.Errorf("%s is gated", path)
			}
		})
	}
}

// With no token configured the board is open by design — a local-only default
// that must not start refusing its owner.
func TestAnUnconfiguredServerIsOpen(t *testing.T) {
	s := authServer(t, "")
	for _, path := range []string{"/api/tasks", "/api/config"} {
		t.Run(path, func(t *testing.T) {
			if res := send(t, s, httptest.NewRequest("GET", path, nil)); res.StatusCode == http.StatusUnauthorized {
				t.Errorf("%s refused a caller on a server with no token", path)
			}
		})
	}
}
