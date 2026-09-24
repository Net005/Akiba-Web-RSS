package main

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const cookieName = "aw_session"

func (s *Server) sessionUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	return s.app.auth.Lookup(c.Value)
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, tok string, exp time.Time, remember bool) {
	c := &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode}
	if remember {
		c.Expires = exp
		c.MaxAge = int(time.Until(exp).Seconds())
	}
	http.SetCookie(w, c)
}

func clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
}

// clientIP trusts X-Forwarded-For only when the peer is a private/loopback
// address (i.e. a reverse proxy on your own network).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return host
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jerr(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

func (s *Server) authState(w http.ResponseWriter, r *http.Request) {
	user, ok := s.sessionUser(r)
	writeJSON(w, 200, map[string]any{
		"needs_setup":   !s.app.auth.HasUser(),
		"authenticated": ok,
		"user":          user,
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	ip := clientIP(r)
	if d, blocked := s.app.limit.blocked(ip); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		jerr(w, http.StatusTooManyRequests, "too many failed attempts — try again in "+d.Round(time.Second).String())
		return
	}
	user, ok := s.app.auth.Verify(b.Username, b.Password)
	if !ok {
		s.app.limit.fail(ip)
		s.app.log.Warn("Failed login for %q from %s", b.Username, ip)
		jerr(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	s.app.limit.ok(ip)
	tok, exp, err := s.app.auth.NewSession(user, b.Remember)
	if err != nil {
		jerr(w, 500, "could not create session")
		return
	}
	s.setCookie(w, r, tok, exp, b.Remember)
	s.app.log.Info("🔓 %s signed in from %s", user, ip)
	writeJSON(w, 200, map[string]any{"ok": true, "user": user})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.app.auth.Destroy(c.Value)
	}
	clearCookie(w, r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// setup creates the first (and only) account. It needs the one-time code
// that is printed in the server log, so that whoever reaches the page first
// on an exposed port cannot claim the instance.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if s.app.auth.HasUser() {
		jerr(w, http.StatusConflict, "an account already exists")
		return
	}
	var b struct {
		Code     string `json:"code"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	ip := clientIP(r)
	if d, blocked := s.app.limit.blocked("setup:" + ip); blocked {
		jerr(w, http.StatusTooManyRequests, "too many attempts — try again in "+d.Round(time.Second).String())
		return
	}
	code := strings.ToUpper(strings.TrimSpace(b.Code))
	if s.app.setupCode == "" || subtle.ConstantTimeCompare([]byte(code), []byte(s.app.setupCode)) != 1 {
		s.app.limit.fail("setup:" + ip)
		jerr(w, http.StatusForbidden, "wrong setup code — it is printed in the server log")
		return
	}
	if err := s.app.auth.SetUser(b.Username, b.Password); err != nil {
		jerr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.app.setupCode = ""
	// make sure settings.json exists so the pipeline can start
	if err := s.app.cfg.Update(map[string]any{}); err != nil {
		s.app.log.Error("could not write settings.json: %v", err)
	}
	s.app.MarkConfigured()
	tok, exp, err := s.app.auth.NewSession(strings.TrimSpace(b.Username), true)
	if err == nil {
		s.setCookie(w, r, tok, exp, true)
	}
	s.app.log.Info("✅ Account %q created; setup complete", strings.TrimSpace(b.Username))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	c, _ := r.Cookie(cookieName)
	keep := ""
	if c != nil {
		keep = c.Value
	}
	if err := s.app.auth.ChangePassword(b.Current, b.New, keep); err != nil {
		jerr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.app.log.Info("🔑 Password changed; other sessions signed out")
	writeJSON(w, 200, map[string]any{"ok": true})
}
