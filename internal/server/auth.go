package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// sessionCookie carries the token for browsers. The CLI sends the same secret
// as a bearer header instead - one secret, two transports, no session table.
const sessionCookie = "fanoutd_session"

// cookieMaxAge keeps a phone logged in for 30 days.
const cookieMaxAge = 30 * 24 * 60 * 60

// publicPaths stay reachable without a token: the health check so a monitor can
// poll it, and the auth endpoints so a browser can obtain a session.
var publicPaths = map[string]bool{
	"/api/health":     true,
	"/api/auth":       true,
	"/api/auth/login": true,
}

func (s *Server) authEnabled() bool { return s.config().Token != "" }

// gated marks the paths a token is required for. Workspace output is served as
// a site under /preview/, so it needs the same gate the API has - a page is no
// less of the board's contents for being HTML.
func gated(path string) bool {
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, previewPrefix)
}

// withAuth gates /api and the served workspaces. The UI's own static files stay
// open so the login page can load before a token exists; everything that reads
// the board or its output is protected.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled() || !gated(r.URL.Path) || publicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if token, ok := strings.CutPrefix(auth, "Bearer "); ok && s.tokenMatches(token) {
			return true
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil && s.tokenMatches(c.Value) {
		return true
	}
	return false
}

func (s *Server) tokenMatches(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.config().Token)) == 1
}

// handleAuth reports whether a token is needed and whether this caller already
// has one, so the UI knows to show a login prompt.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{
		"required":      s.authEnabled(),
		"authenticated": !s.authEnabled() || s.authorized(r),
	})
}

func (s *Server) handleAuthRoute(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/auth/login":
		s.handleLogin(w, r)
	case "/api/auth/logout":
		s.handleLogout(w, r)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authEnabled() {
		http.Error(w, "authentication is not enabled", http.StatusBadRequest)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !s.tokenMatches(strings.TrimSpace(req.Token)) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.config().Token,
		Path:     "/",
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"authenticated": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// isTLS marks the cookie Secure when the connection is https, including behind
// a reverse proxy that terminates TLS for us.
func isTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
