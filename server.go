package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	app *App
	mux *http.ServeMux
}

// publicPath lists what needs no session: the RSS feed (readers can't log
// in), health, the login/setup pages and their assets.
func publicPath(p string) bool {
	switch p {
	case "/health", "/login", "/setup", "/login.html", "/setup.html", "/style.css",
		"/api/auth/state", "/api/auth/login", "/api/auth/setup":
		return true
	}
	if p == "/giga/feed" || strings.HasPrefix(p, "/giga/feed/") {
		// the legacy blocking refresh endpoints trigger a scrape: keep them private
		return !strings.HasSuffix(p, "/realtime") && !strings.HasSuffix(p, "/refresh")
	}
	return false
}

func NewServer(app *App) *Server {
	s := &Server{app: app, mux: http.NewServeMux()}
	sub, _ := fs.Sub(webFS, "web")
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
	s.mux.HandleFunc("/login", s.page("login.html"))
	s.mux.HandleFunc("/setup", s.page("setup.html"))

	// Public, backwards-compatible endpoints (RSS readers, monitoring).
	s.mux.HandleFunc("/giga/feed", s.feed)
	s.mux.HandleFunc("/giga/feed/", s.feed)
	s.mux.HandleFunc("/giga/feed/raw", s.feedRaw)
	s.mux.HandleFunc("/health", s.health)

	// Legacy blocking refresh endpoints (session required).
	s.mux.HandleFunc("/giga/feed/realtime", s.legacyRefresh(true))
	s.mux.HandleFunc("/giga/feed/refresh", s.legacyRefresh(false))

	api := map[string]http.HandlerFunc{
		"GET /api/auth/state":      s.authState,
		"POST /api/auth/login":     s.login,
		"POST /api/auth/setup":     s.setup,
		"POST /api/auth/logout":    s.logout,
		"POST /api/auth/password":  s.changePassword,
		"GET /api/status":          s.status,
		"GET /api/releases":        s.releases,
		"GET /api/history":         s.history,
		"GET /api/logs":            s.logs,
		"GET /api/logs/stream":     s.logStream,
		"GET /api/logs/download":   s.logDownload,
		"POST /api/logs/clear":     s.logClear,
		"POST /api/run":            s.run,
		"POST /api/stop":           s.stop,
		"GET /api/jd":              s.jdInfo,
		"POST /api/jd/reconnect":   s.jdReconnect,
		"GET /api/config":          s.getConfig,
		"POST /api/config":         s.setConfig,
		"POST /api/pushover/test":  s.pushoverTest,
		"GET /api/state":           s.stateInfo,
		"POST /api/release/action": s.releaseAction,
	}
	for pat, h := range api {
		s.mux.HandleFunc(pat, h)
	}
	return s
}

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := webFS.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	}
}

type ctxKey int

const userKey ctxKey = 1

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
			// CSRF guard for state-changing requests: same-origin only.
			if r.Method == http.MethodPost {
				if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
					jerr(w, http.StatusForbidden, "cross-origin request refused")
					return
				}
			}
		}
		user, authed := s.sessionUser(r)
		if authed {
			r = r.WithContext(context.WithValue(r.Context(), userKey, user))
		}
		p := r.URL.Path
		switch {
		case publicPath(p):
			// signed-in users have no business on the login page
			if authed && (p == "/login" || p == "/login.html") {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
		case authed:
		case !s.app.auth.HasUser():
			if strings.HasPrefix(p, "/api/") {
				jerr(w, http.StatusUnauthorized, "setup required")
			} else {
				http.Redirect(w, r, "/setup", http.StatusFound)
			}
			return
		case strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/giga/"):
			jerr(w, http.StatusUnauthorized, "authentication required")
			return
		default:
			next := ""
			if r.Method == http.MethodGet && p != "/" {
				next = "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, "/login"+next, http.StatusFound)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jerr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// ── Public endpoints ─────────────────────────────────────────────

func (s *Server) feedAge() time.Duration {
	s.app.mu.Lock()
	t := s.app.lastFeed
	s.app.mu.Unlock()
	if t.IsZero() {
		if fi, err := os.Stat(s.app.cfg.Get().RSSFile); err == nil {
			t = fi.ModTime()
		}
	}
	if t.IsZero() {
		return 100 * 24 * time.Hour // no feed yet
	}
	return time.Since(t)
}

func (s *Server) serveFeed(w http.ResponseWriter, ctype string) {
	b, err := os.ReadFile(s.app.cfg.Get().RSSFile)
	if err != nil {
		http.Error(w, "feed not generated yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(b)
}

func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	if s.feedAge() > 2*time.Hour {
		s.app.Trigger("feed request")
	}
	s.serveFeed(w, "application/rss+xml; charset=utf-8")
}

func (s *Server) feedRaw(w http.ResponseWriter, r *http.Request) {
	s.serveFeed(w, "application/xml; charset=utf-8")
}

func (s *Server) legacyRefresh(jsonOut bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.app.RunPipeline("legacy refresh")
		if jsonOut {
			writeJSON(w, 200, map[string]any{"status": "scraped", "path": s.app.cfg.Get().RSSFile})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<h1>Feed refreshed!</h1>")
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	conn, devs, _ := s.app.JDInfo()
	var found, queued int
	s.app.st.Read(func(st *State) { found, queued = len(st.FoundLinks), len(st.QueuedReleases) })
	writeJSON(w, 200, map[string]any{
		"status":                "ok",
		"jd_connected":          conn,
		"jd_devices":            len(devs),
		"state_found_links":     found,
		"state_queued_releases": queued,
		"rss_file":              s.app.cfg.Get().RSSFile,
		"uptime":                int(time.Since(s.app.start).Seconds()),
		"cache_age_seconds":     s.feedAge().Seconds(),
	})
}

// ── Control API ──────────────────────────────────────────────────

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	c := s.app.cfg.Get()
	conn, devs, jerrMsg := s.app.JDInfo()
	var found, queued, notified, pending int
	s.app.st.Read(func(st *State) {
		found, queued, notified, pending = len(st.FoundLinks), len(st.QueuedReleases), len(st.NotifiedReleases), len(st.PendingNotifications)
	})
	rels := s.app.Releases()
	counts := map[string]int{}
	for _, v := range rels {
		counts[v.State]++
	}
	var forumPage int
	s.app.st.Read(func(st *State) { forumPage = st.ForumMaxPage })
	writeJSON(w, 200, map[string]any{
		"pipeline": s.app.GetStatus(),
		"uptime":   int(time.Since(s.app.start).Seconds()),
		"now":      time.Now().UTC().Format(time.RFC3339),
		"jd":       map[string]any{"connected": conn, "devices": len(devs), "error": jerrMsg, "has_credentials": c.MyJDEmail != ""},
		"counts": map[string]any{
			"releases": len(rels), "by_state": counts,
			"found_links": found, "queued_releases": queued,
			"notified": notified, "pending_notifications": pending,
		},
		"pushover":       len(c.PushoverDestinations),
		"forum_max_page": forumPage,
		"feed_age":       s.feedAge().Seconds(),
		"interval":       c.ScheduleInterval,
		"user":           r.Context().Value(userKey),
		"imported":       s.app.Imported,
		"needs_myjd":     c.MyJDEmail == "" || c.MyJDPassword == "",
	})
}

func (s *Server) releases(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.app.Releases())
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.app.History())
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	writeJSON(w, 200, s.app.log.Snapshot(since, limit))
}

func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n
		}
	}
	ch, cancel := s.app.log.Subscribe()
	defer cancel()
	send := func(e LogEntry) {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, b)
	}
	last := since
	for _, e := range s.app.log.Snapshot(since, 0) {
		send(e)
		last = e.ID
	}
	fl.Flush()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if e.ID <= last {
				continue
			}
			send(e)
			last = e.ID
			// drain burst before flushing
			for drained := false; !drained; {
				select {
				case e2 := <-ch:
					if e2.ID > last {
						send(e2)
						last = e2.ID
					}
				default:
					drained = true
				}
			}
			fl.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) logDownload(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(s.app.log.FilePath())
	if err != nil {
		http.Error(w, "no log file", 404)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="scraper.log"`)
	_, _ = w.Write(b)
}

func (s *Server) logClear(w http.ResponseWriter, r *http.Request) {
	s.app.log.Clear()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	if s.app.GetStatus().Running {
		jerr(w, 409, "a run is already in progress")
		return
	}
	s.app.Trigger("manual")
	writeJSON(w, 202, map[string]any{"ok": true})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if !s.app.RequestStop() {
		jerr(w, 409, "nothing is running")
		return
	}
	s.app.log.Warn("Stop requested from control panel")
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) jdInfo(w http.ResponseWriter, r *http.Request) {
	conn, devs, e := s.app.JDInfo()
	writeJSON(w, 200, map[string]any{"connected": conn, "devices": devs, "error": e})
}

func (s *Server) jdReconnect(w http.ResponseWriter, r *http.Request) {
	ok := s.app.InitMyJD()
	_, devs, e := s.app.JDInfo()
	writeJSON(w, 200, map[string]any{"ok": ok, "devices": devs, "error": e})
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.app.cfg.Redacted())
}

func (s *Server) setConfig(w http.ResponseWriter, r *http.Request) {
	var patch map[string]any
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		jerr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	before := s.app.cfg.Get()
	if err := s.app.cfg.Update(patch); err != nil {
		jerr(w, 400, err.Error())
		return
	}
	after := s.app.cfg.Get()
	s.app.MarkConfigured()
	s.app.log.Info("⚙️ Settings saved")
	restart := before.Port != after.Port || before.ListenHost != after.ListenHost ||
		before.LogFile != after.LogFile || before.StateFile != after.StateFile
	if before.ScheduleInterval != after.ScheduleInterval {
		s.app.Reschedule()
	}
	if before.MyJDEmail != after.MyJDEmail || before.MyJDPassword != after.MyJDPassword {
		go s.app.InitMyJD()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "restart_required": restart})
}

func (s *Server) pushoverTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Index int `json:"index"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.app.TestPushover(body.Index); err != nil {
		jerr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) stateInfo(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Link string `json:"link"`
	}
	var links []string
	var queued, notified map[string]string
	var pending []string
	s.app.st.Read(func(st *State) {
		for l := range st.FoundLinks {
			links = append(links, l)
		}
		queued, notified = map[string]string{}, map[string]string{}
		for k, v := range st.QueuedReleases {
			queued[k] = v
		}
		for k, v := range st.NotifiedReleases {
			notified[k] = v
		}
		for k := range st.PendingNotifications {
			pending = append(pending, k)
		}
	})
	sort.Strings(links)
	sort.Strings(pending)
	writeJSON(w, 200, map[string]any{"found_links": links, "queued_releases": queued, "notified_releases": notified, "pending_notifications": pending})
}

func (s *Server) releaseAction(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.ID == "" {
		jerr(w, 400, "bad request")
		return
	}
	switch b.Action {
	case "queue":
		n, err := s.app.QueueRelease(b.ID)
		if err != nil {
			jerr(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "queued": n})
	case "forget":
		if err := s.app.ForgetRelease(b.ID, true, true); err != nil {
			jerr(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case "renotify":
		ok, err := s.app.Renotify(b.ID)
		if err != nil {
			jerr(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": ok})
	default:
		jerr(w, 400, "unknown action")
	}
}
