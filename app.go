package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunRecord is one pipeline run in the history.
type RunRecord struct {
	ID        int     `json:"id"`
	Reason    string  `json:"reason"`
	Started   string  `json:"started"`
	Seconds   float64 `json:"seconds"`
	OK        bool    `json:"ok"`
	Error     string  `json:"error,omitempty"`
	Releases  int     `json:"releases"`
	Matched   int     `json:"matched"`
	Queued    int     `json:"queued_links"`
	Notified  int     `json:"notifications"`
	ForumOK   bool    `json:"forum_ok"`
	ForumPage int     `json:"forum_max_page"`
}

// Status is the live pipeline status for the UI.
type Status struct {
	Running     bool       `json:"running"`
	Phase       string     `json:"phase"`
	Current     string     `json:"current"`
	Done        int        `json:"done"`
	Total       int        `json:"total"`
	Reason      string     `json:"reason"`
	StartedAt   string     `json:"started_at,omitempty"`
	NextRun     string     `json:"next_run,omitempty"`
	LastRun     *RunRecord `json:"last_run,omitempty"`
	StopPending bool       `json:"stop_pending"`
}

type App struct {
	cfg   *ConfigStore
	log   *Hub
	st    *StateStore
	push  *PushoverSender
	start time.Time

	runMu sync.Mutex // held while a pipeline runs

	mu        sync.Mutex // guards fields below
	status    Status
	history   []RunRecord
	nextRunID int
	records   []ReleaseRecord
	stop      bool
	lastFeed  time.Time

	jdMu    sync.Mutex
	jd      *MyJD
	devices []JDDevice
	jdErr   string
	rr      int

	kick chan struct{} // reschedule scheduler
	trig chan string   // manual/feed triggers

	auth      *AuthStore
	limit     *limiter
	setupCode string // one-time code printed to the log while no account exists
	ready     chan struct{}
	readyOnce sync.Once
	Imported  bool // a legacy config.json was imported at startup

	coversDir string // local cache of cover/thumbnail images, served at /covers/
}

// MarkConfigured releases the startup gate once settings exist.
func (a *App) MarkConfigured() { a.readyOnce.Do(func() { close(a.ready) }) }

// WaitConfigured blocks until settings.json exists (setup finished/imported).
func (a *App) WaitConfigured() { <-a.ready }

func NewApp(cfg *ConfigStore, log *Hub) *App {
	c := cfg.Get()
	coversDir := filepath.Join(cfg.dir, "covers")
	if err := os.MkdirAll(coversDir, 0o755); err != nil {
		log.Warn("Could not create cover cache directory %s: %v", coversDir, err)
	}
	app := &App{
		cfg: cfg, log: log, st: NewStateStore(c.StateFile), push: NewPushoverSender(),
		start: time.Now(), jd: NewMyJD(),
		kick: make(chan struct{}, 1), trig: make(chan string, 4),
		auth: NewAuthStore(filepath.Join(cfg.dir, "auth.json")), limit: newLimiter(),
		ready:     make(chan struct{}),
		coversDir: coversDir,
	}
	if err := app.auth.Load(); err != nil {
		log.Error("auth.json could not be read: %v", err)
	}
	if !app.auth.HasUser() {
		app.setupCode = randomCode()
	}
	if cfg.Exists() {
		app.MarkConfigured()
	}
	return app
}

// ── persistence of releases + history ─────────────────────────────

func (a *App) loadCaches() {
	c := a.cfg.Get()
	if b, err := os.ReadFile(c.ReleasesFile); err == nil {
		_ = json.Unmarshal(b, &a.records)
	}
	if b, err := os.ReadFile(c.HistoryFile); err == nil {
		_ = json.Unmarshal(b, &a.history)
		for _, h := range a.history {
			if h.ID > a.nextRunID {
				a.nextRunID = h.ID
			}
		}
		if n := len(a.history); n > 0 {
			l := a.history[n-1]
			a.status.LastRun = &l
		}
	}
}

func writeJSONFile(path string, v any) {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// loadState (re)loads the state file. Never fatal: a missing or invalid file
// is replaced by a fresh state.
func (a *App) loadState() {
	if err := a.st.Load(); err != nil {
		a.log.Error("State load failed: %v", err)
		return
	}
	if r := a.st.Recovered; r != "" {
		a.log.Warn("⚠️ %s", r)
		a.st.Recovered = ""
	}
}

// ── MyJD ────────────────────────────────────────────────────────

func (a *App) InitMyJD() bool {
	a.jdMu.Lock()
	defer a.jdMu.Unlock()
	return a.initMyJDLocked()
}

func (a *App) initMyJDLocked() bool {
	c := a.cfg.Get()
	if c.MyJDEmail == "" || c.MyJDPassword == "" {
		a.log.Warn("No MyJD credentials in config, MyJD disabled")
		a.jdErr = "no credentials configured"
		a.devices = nil
		return false
	}
	jd := NewMyJD()
	if err := jd.Connect(c.MyJDEmail, c.MyJDPassword); err != nil {
		a.log.Error("MyJD connection failed: %v", err)
		a.jdErr = err.Error()
		a.jd, a.devices = jd, nil
		return false
	}
	devs, err := jd.ListDevices()
	if err != nil {
		a.log.Error("MyJD connection failed: %v", err)
		a.jdErr = err.Error()
		a.devices = nil
		return false
	}
	sortDevices(devs)
	a.jd, a.devices, a.jdErr = jd, devs, ""
	a.log.Info("MyJD: Connected, %d devices found (ordered by trailing number)", len(devs))
	for i, d := range devs {
		if n, ok := deviceNumber(d.Name); ok {
			a.log.Info("  - [%d] %s (#%d)", i+1, d.Name, n)
		} else {
			a.log.Info("  - [%d] %s (#N/A)", i+1, d.Name)
		}
	}
	return true
}

func (a *App) JDInfo() (connected bool, devices []JDDevice, errMsg string) {
	a.jdMu.Lock()
	defer a.jdMu.Unlock()
	return a.jd != nil && a.jd.Connected() && len(a.devices) > 0, append(make([]JDDevice, 0, len(a.devices)), a.devices...), a.jdErr
}

func (a *App) addLinkToDevice(dev JDDevice, link, releaseID string) error {
	_, err := a.jd.Call(dev.ID, "/linkgrabberv2/addLinks", map[string]any{
		"autostart":   true,
		"links":       link,
		"packageName": releaseID,
	})
	return err
}

// addLinksToJD ports add_links_to_jd: each link goes to a different device
// (1→2→3→1…). Returns number of links successfully queued.
func (a *App) addLinksToJD(links []string, releaseID string) int {
	a.jdMu.Lock()
	defer a.jdMu.Unlock()
	if a.jd == nil || !a.jd.Connected() || len(a.devices) == 0 {
		a.log.Warn("❌ %s — no JD devices, storing links but not forwarding", releaseID)
		return 0
	}
	hasNumbers := false
	for _, d := range a.devices {
		if n, ok := deviceNumber(d.Name); ok && n != 0 {
			hasNumbers = true
		}
	}
	success := 0
	for idx, link := range links {
		var already bool
		a.st.Read(func(s *State) { already = s.FoundLinks[link] })
		if already {
			a.log.Info("⏭️ %s — link %d already queued, skipping", releaseID, idx+1)
			continue
		}
		var dev JDDevice
		if hasNumbers {
			dev = a.devices[idx%len(a.devices)]
		} else {
			dev = a.devices[a.rr%len(a.devices)]
			a.rr++
		}
		if idx > 0 {
			time.Sleep(3*time.Second + time.Duration(rand.Intn(2000))*time.Millisecond)
		}
		err := a.addLinkToDevice(dev, link, releaseID)
		if isTokenInvalid(err) {
			a.log.Warn("MyJD session expired; reconnecting before retrying %s link %d/%d", releaseID, idx+1, len(links))
			if a.initMyJDLocked() {
				// device list may have been re-sorted; re-find by name
				for _, d := range a.devices {
					if d.Name == dev.Name {
						dev = d
					}
				}
				err = a.addLinkToDevice(dev, link, releaseID)
			} else {
				err = errors.New("MyJD session expired and reconnect failed")
			}
		}
		switch {
		case err == nil:
			suffix := "round-robin"
			if hasNumbers {
				n, _ := deviceNumber(dev.Name)
				suffix = fmt.Sprintf("#%d", n)
			}
			a.log.Info("✅ %s — link %d/%d → %s (%s)", releaseID, idx+1, len(links), dev.Name, suffix)
			_ = a.st.With(func(s *State) { s.FoundLinks[link] = true })
			success++
		case isDuplicate(err):
			a.log.Info("✅ %s — link %d/%d already exists on %s; marked successful", releaseID, idx+1, len(links), dev.Name)
			_ = a.st.With(func(s *State) { s.FoundLinks[link] = true })
			success++
		default:
			a.log.Error("❌ %s — link %d/%d failed on %s: %v", releaseID, idx+1, len(links), dev.Name, err)
		}
	}
	a.log.Info("ℹ️ %s — %d/%d links successfully queued", releaseID, success, len(links))
	if success == len(links) {
		_ = a.st.With(func(s *State) { s.QueuedReleases[releaseID] = time.Now().UTC().Format(time.RFC3339Nano) })
	}
	return success
}

// ── Pushover ────────────────────────────────────────────────────

func (a *App) notify(rec ReleaseRecord) (allOK bool, sent int) {
	c := a.cfg.Get()
	id := rec.ID
	var already bool
	a.st.Read(func(s *State) { _, already = s.NotifiedReleases[id] })
	if already {
		a.log.Info("⏭️ %s — Pushover notification already sent", id)
		return true, 0
	}
	if len(c.PushoverDestinations) == 0 {
		a.log.Info("ℹ️ %s — no Pushover destinations configured", id)
		return false, 0
	}
	allOK = true
	var cover *coverBlob
	for i, d := range c.PushoverDestinations {
		did := destID(d)
		var done bool
		a.st.Read(func(s *State) { _, done = s.PushoverDeliveries[id][did] })
		if done {
			continue
		}
		if d.APIToken == "" || d.UserKey == "" {
			a.log.Error("❌ %s — Pushover destination %d is missing api_token or user_key", id, i+1)
			allOK = false
			continue
		}
		if err := a.push.Send(d, rec.RSSTitle, rec.Detail.Story, rec.Detail.Cover, rec.URL, &cover); err != nil {
			allOK = false
			a.log.Error("❌ %s — Pushover destination %d failed: %v", id, i+1, err)
			continue
		}
		a.log.Info("🔔 %s — Pushover destination %d notified", id, i+1)
		sent++
		_ = a.st.With(func(s *State) {
			if s.PushoverDeliveries[id] == nil {
				s.PushoverDeliveries[id] = map[string]string{}
			}
			s.PushoverDeliveries[id][did] = time.Now().UTC().Format(time.RFC3339Nano)
		})
	}
	if allOK {
		_ = a.st.With(func(s *State) {
			s.NotifiedReleases[id] = time.Now().UTC().Format(time.RFC3339Nano)
			delete(s.PendingNotifications, id)
		})
	}
	return allOK, sent
}

func (a *App) TestPushover(index int) error {
	c := a.cfg.Get()
	if index < 0 || index >= len(c.PushoverDestinations) {
		return errors.New("no such destination")
	}
	d := c.PushoverDestinations[index]
	err := a.push.SendRaw(d, "Test notification from Akiba-Web RSS control panel")
	if err != nil {
		a.log.Error("Pushover test to destination %d failed: %v", index+1, err)
		return err
	}
	a.log.Info("🔔 Pushover test sent to destination %d", index+1)
	return nil
}

// ── Pipeline ────────────────────────────────────────────────────

func (a *App) setStatus(fn func(s *Status)) {
	a.mu.Lock()
	fn(&a.status)
	a.mu.Unlock()
}

func (a *App) GetStatus() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *App) stopRequested() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stop
}

// RequestStop asks a running pipeline to stop after the current release.
func (a *App) RequestStop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.status.Running {
		return false
	}
	a.stop = true
	a.status.StopPending = true
	return true
}

// RunPipeline runs at most one pipeline at a time. Returns false if one is
// already running.
func (a *App) RunPipeline(reason string) bool {
	if !a.runMu.TryLock() {
		a.log.Info("⏭️ Pipeline already running; duplicate request skipped")
		return false
	}
	defer a.runMu.Unlock()

	started := time.Now()
	a.mu.Lock()
	a.stop = false
	a.status.Running, a.status.Reason = true, reason
	a.status.Phase, a.status.Current = "starting", ""
	a.status.Done, a.status.Total = 0, 0
	a.status.StopPending = false
	a.status.StartedAt = started.UTC().Format(time.RFC3339)
	a.nextRunID++
	rec := RunRecord{ID: a.nextRunID, Reason: reason, Started: started.UTC().Format(time.RFC3339)}
	a.mu.Unlock()

	err := a.runPipeline(&rec)

	rec.Seconds = time.Since(started).Seconds()
	rec.OK = err == nil
	if err != nil {
		rec.Error = err.Error()
	}
	a.mu.Lock()
	a.history = append(a.history, rec)
	if len(a.history) > 100 {
		a.history = a.history[len(a.history)-100:]
	}
	r := rec
	a.status.LastRun = &r
	a.status.Running, a.status.Phase, a.status.Current = false, "idle", ""
	a.status.StopPending = false
	hist := append([]RunRecord(nil), a.history...)
	a.mu.Unlock()
	writeJSONFile(a.cfg.Get().HistoryFile, hist)
	return true
}

func (a *App) runPipeline(rec *RunRecord) error {
	c := a.cfg.Get()
	a.log.Info("═══ Pipeline run started (%s) ═══", rec.Reason)
	a.loadState()
	var n int
	a.st.Read(func(s *State) { n = len(s.FoundLinks) })
	a.log.Info("Loaded state: %d previously found", n)

	a.setStatus(func(s *Status) { s.Phase = "akiba: fetching listing" })
	akiba := NewAkibaScraper(c.AkibaBase, a.log)
	releases, err := akiba.FetchReleases(c.AkibaReleasesPath)
	if err != nil || len(releases) == 0 {
		if err == nil {
			err = errors.New("no releases found on listing")
		}
		a.log.Error("Homepage fetch failed: %v", err)
		return err
	}
	rec.Releases = len(releases)

	a.setStatus(func(s *Status) { s.Phase = "forum: discovering links" })
	forum := NewForumScraper(c.ForumBase, c.ForumThread, c.ForumPages, c.ForumRetries, a.log)
	var lastKnown int
	a.st.Read(func(s *State) { lastKnown = s.ForumMaxPage })
	forumLinks, maxPage, ferr := forum.FetchLinks(lastKnown)
	rec.ForumOK = ferr == nil
	if ferr != nil {
		a.log.Error("Forum step skipped: %v — keeping previously known links, will retry next run", ferr)
	} else if maxPage > 0 {
		rec.ForumPage = maxPage
		_ = a.st.With(func(s *State) { s.ForumMaxPage = maxPage })
	}

	akibaIDs, forumIDs, matched := map[string]bool{}, map[string]bool{}, []string{}
	for _, r := range releases {
		akibaIDs[r.ID] = true
	}
	for k := range forumLinks {
		forumIDs[k] = true
	}
	for k := range akibaIDs {
		if forumIDs[k] {
			matched = append(matched, k)
		}
	}
	sort.Strings(matched)
	rec.Matched = len(matched)
	if len(matched) > 0 {
		a.log.Info("Matched IDs (will download): %s", strings.Join(matched, ", "))
	} else if ferr == nil {
		a.log.Warn("⚠️ No matches! Forum has %d IDs but none match Akiba's %d.", len(forumIDs), len(akibaIDs))
	}

	// cached records for fallback
	a.mu.Lock()
	cache := map[string]ReleaseRecord{}
	for _, r := range a.records {
		cache[r.ID] = r
	}
	a.mu.Unlock()

	a.setStatus(func(s *Status) { s.Phase = "processing releases"; s.Total = len(releases) })
	var out []ReleaseRecord
	for i, rel := range releases {
		if a.stopRequested() {
			a.log.Warn("Pipeline stopped by user after %d/%d releases", i, len(releases))
			// keep the rest from cache so the feed doesn't shrink
			for _, r := range releases[i:] {
				if c, ok := cache[r.ID]; ok {
					out = append(out, c)
				}
			}
			break
		}
		a.setStatus(func(s *Status) { s.Current = rel.ID; s.Done = i })
		a.log.Info("Processing %s...", rel.ID)
		det, derr := akiba.FetchDetail(rel.URL)
		old, haveOld := cache[rel.ID]
		var detail Detail
		switch {
		case derr == nil && det != nil:
			detail = *det
		case haveOld:
			a.log.Warn("Product detail fetch failed for %s: %v — using cached copy", rel.ID, derr)
			detail = old.Detail
		default:
			a.log.Error("Product detail fetch failed for %s: %v", rel.URL, derr)
			continue
		}
		links := forumLinks[rel.ID]
		if ferr != nil && haveOld {
			links = old.Links
		}
		if detail.Cover != "" {
			detail.Cover = a.cacheImage(rel.ID, "cover", detail.Cover)
		}
		thumb := a.cacheImage(rel.ID, "thumb", rel.Thumbnail)
		rssTitle := fmt.Sprintf("[%s] %s", rel.ID, detail.Title)
		record := ReleaseRecord{
			ID: rel.ID, URL: rel.URL, Thumbnail: thumb, RSSTitle: rssTitle,
			Detail: detail, Links: links, UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		pub := rel.ListingReleased
		if pub == "" {
			pub = detail.ReleaseDate
		}
		if t, e := time.Parse("2006/01/02", strings.TrimSpace(pub)); e == nil {
			record.PubDate = t.UTC().Format(time.RFC3339)
		} else if haveOld && old.PubDate != "" {
			record.PubDate = old.PubDate
		} else {
			record.PubDate = time.Now().UTC().Format(time.RFC3339)
		}

		if ferr == nil {
			a.deliver(&record, rec)
		} else if len(links) == 0 {
			a.log.Info("⏸️ %s — forum unavailable this run, skipping link handling", rel.ID)
		}
		out = append(out, record)
	}
	a.setStatus(func(s *Status) { s.Done = len(releases); s.Phase = "writing feed" })

	if len(out) == 0 {
		return errors.New("no release could be processed; feed left untouched")
	}

	// Merge this run's releases into the full archive instead of replacing
	// it, so releases that have scrolled off Akiba-Web's listing page are
	// never forgotten — only the current run's items get fresh data.
	a.mu.Lock()
	archive := make(map[string]ReleaseRecord, len(a.records)+len(out))
	for _, r := range a.records {
		archive[r.ID] = r
	}
	for _, r := range out {
		archive[r.ID] = r
	}
	a.mu.Unlock()
	merged := make([]ReleaseRecord, 0, len(archive))
	for _, r := range archive {
		merged = append(merged, r)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].PubDate > merged[j].PubDate })

	const maxFeedItems = 200
	feedRecs := merged
	if len(feedRecs) > maxFeedItems {
		feedRecs = feedRecs[:maxFeedItems]
	}
	if err := a.writeFeed(feedRecs); err != nil {
		a.log.Error("RSS write failed: %v", err)
		return err
	}
	a.mu.Lock()
	a.records = merged
	a.lastFeed = time.Now()
	a.mu.Unlock()
	writeJSONFile(c.ReleasesFile, merged)
	a.log.Info("═══ Pipeline complete: %d items this run, %d in archive, %d downloads matched, RSS saved to %s ═══", len(out), len(merged), len(matched), c.RSSFile)
	return nil
}

var coverClient = &http.Client{Timeout: 20 * time.Second}

// cacheImage downloads url into the local cover cache (once) and returns a
// "/covers/<file>" path to serve it from; on any failure, or if it is
// already a locally cached path, the original url is returned unchanged so
// the frontend always has something to point at.
func (a *App) cacheImage(id, kind, url string) string {
	if url == "" || a.coversDir == "" || strings.HasPrefix(url, "/covers/") {
		return url
	}
	ext := filepath.Ext(strings.SplitN(url, "?", 2)[0])
	if ext == "" || len(ext) > 5 {
		ext = ".jpg"
	}
	name := sanitizeID(id) + "-" + kind + ext
	path := filepath.Join(a.coversDir, name)
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		return "/covers/" + name
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return url
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; AkibaWebRSS/1.0)")
	resp, err := coverClient.Do(req)
	if err != nil {
		a.log.Warn("Cover download failed for %s (%s): %v", id, kind, err)
		return url
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		a.log.Warn("Cover download failed for %s (%s): HTTP %d", id, kind, resp.StatusCode)
		return url
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return url
	}
	_, cerr := io.Copy(f, resp.Body)
	f.Close()
	if cerr != nil {
		os.Remove(tmp)
		return url
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return url
	}
	return "/covers/" + name
}

func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "id"
	}
	return b.String()
}

// deliver handles JD queueing + notification for one release (ported logic).
func (a *App) deliver(record *ReleaseRecord, run *RunRecord) {
	id := record.ID
	if len(record.Links) == 0 {
		a.log.Info("❌ %s — no forum links found yet, will retry next run", id)
		return
	}
	var pending []string
	a.st.Read(func(s *State) {
		for _, l := range record.Links {
			if !s.FoundLinks[l] {
				pending = append(pending, l)
			}
		}
	})
	if len(pending) == 0 {
		a.log.Info("⏭️ %s — all links already queued, skipping", id)
	} else {
		a.log.Info("✅ %s — %d new links, %d already done", id, len(pending), len(record.Links)-len(pending))
		if ok, _, _ := a.JDInfo(); ok {
			if q := a.addLinksToJD(pending, id); q > 0 {
				run.Queued += q
				_ = a.st.With(func(s *State) { s.PendingNotifications[id] = true })
			}
		} else {
			a.log.Warn("⚠️ %s — no JD devices", id)
		}
	}
	var pend bool
	a.st.Read(func(s *State) { pend = s.PendingNotifications[id] })
	if pend {
		_, sent := a.notify(*record)
		run.Notified += sent
	}
}

func (a *App) writeFeed(records []ReleaseRecord) error {
	c := a.cfg.Get()
	self := c.PublicBaseURL
	if self == "" {
		self = fmt.Sprintf("http://localhost:%d/", c.Port)
	}
	b, err := renderRSS(records, self)
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(c.RSSFile), 0o755)
	tmp := c.RSSFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.RSSFile)
}

// ── Manual operations used by the web UI ─────────────────────────

func (a *App) findRecord(id string) (ReleaseRecord, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.records {
		if r.ID == id {
			return r, true
		}
	}
	return ReleaseRecord{}, false
}

// QueueRelease sends any not-yet-queued links of a release to JD right now.
func (a *App) QueueRelease(id string) (int, error) {
	rec, ok := a.findRecord(id)
	if !ok {
		return 0, errors.New("unknown release")
	}
	if !a.runMu.TryLock() {
		return 0, errors.New("pipeline is running; try again when it is idle")
	}
	defer a.runMu.Unlock()
	a.loadState()
	var pending []string
	a.st.Read(func(s *State) {
		for _, l := range rec.Links {
			if !s.FoundLinks[l] {
				pending = append(pending, l)
			}
		}
	})
	if len(pending) == 0 {
		return 0, errors.New("all links already queued")
	}
	if ok, _, _ := a.JDInfo(); !ok {
		return 0, errors.New("MyJDownloader is not connected")
	}
	n := a.addLinksToJD(pending, id)
	if n > 0 {
		_ = a.st.With(func(s *State) { s.PendingNotifications[id] = true })
	}
	return n, nil
}

// ForgetRelease clears queue/notification state so the next run re-queues it.
func (a *App) ForgetRelease(id string, links bool, notify bool) error {
	rec, _ := a.findRecord(id)
	if !a.runMu.TryLock() {
		return errors.New("pipeline is running; try again when it is idle")
	}
	defer a.runMu.Unlock()
	a.loadState()
	err := a.st.With(func(s *State) {
		if links {
			delete(s.QueuedReleases, id)
			for _, l := range rec.Links {
				delete(s.FoundLinks, l)
			}
		}
		if notify {
			delete(s.NotifiedReleases, id)
			delete(s.PushoverDeliveries, id)
			if links {
				delete(s.PendingNotifications, id)
			}
		}
	})
	a.log.Info("🧹 %s — state cleared (links=%v notification=%v)", id, links, notify)
	return err
}

// Renotify resets the notification for a release and sends it again now.
func (a *App) Renotify(id string) (bool, error) {
	rec, ok := a.findRecord(id)
	if !ok {
		return false, errors.New("unknown release")
	}
	if !a.runMu.TryLock() {
		return false, errors.New("pipeline is running; try again when it is idle")
	}
	defer a.runMu.Unlock()
	a.loadState()
	_ = a.st.With(func(s *State) {
		delete(s.NotifiedReleases, id)
		delete(s.PushoverDeliveries, id)
		s.PendingNotifications[id] = true
	})
	ok2, _ := a.notify(rec)
	return ok2, nil
}

// ReleaseView is a record plus its live state for the UI.
type ReleaseView struct {
	ReleaseRecord
	LinksTotal    int    `json:"links_total"`
	LinksQueued   int    `json:"links_queued"`
	QueuedAt      string `json:"queued_at,omitempty"`
	NotifiedAt    string `json:"notified_at,omitempty"`
	PendingNotify bool   `json:"pending_notify"`
	State         string `json:"state"` // queued | partial | waiting | nolinks
}

func (a *App) Releases() []ReleaseView {
	a.mu.Lock()
	recs := append([]ReleaseRecord(nil), a.records...)
	a.mu.Unlock()
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].PubDate > recs[j].PubDate })
	out := make([]ReleaseView, 0, len(recs))
	a.st.Read(func(s *State) {
		for _, r := range recs {
			v := ReleaseView{ReleaseRecord: r, LinksTotal: len(r.Links)}
			for _, l := range r.Links {
				if s.FoundLinks[l] {
					v.LinksQueued++
				}
			}
			v.QueuedAt = s.QueuedReleases[r.ID]
			v.NotifiedAt = s.NotifiedReleases[r.ID]
			v.PendingNotify = s.PendingNotifications[r.ID]
			switch {
			case v.LinksTotal == 0:
				v.State = "nolinks"
			case v.LinksQueued == v.LinksTotal:
				v.State = "queued"
			case v.LinksQueued > 0:
				v.State = "partial"
			default:
				v.State = "waiting"
			}
			out = append(out, v)
		}
	})
	return out
}

func (a *App) History() []RunRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	h := append(make([]RunRecord, 0, len(a.history)), a.history...)
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return h
}

// ── Scheduler ───────────────────────────────────────────────────

func (a *App) SchedulerLoop() {
	for {
		iv := time.Duration(a.cfg.Get().ScheduleInterval) * time.Second
		next := time.Now().Add(iv)
		a.setStatus(func(s *Status) { s.NextRun = next.UTC().Format(time.RFC3339) })
		select {
		case <-time.After(iv):
			a.RunPipeline("schedule")
		case reason := <-a.trig:
			a.RunPipeline(reason)
		case <-a.kick:
			// interval changed → recompute
		}
	}
}

// Trigger requests a run asynchronously (deduplicated).
func (a *App) Trigger(reason string) {
	select {
	case a.trig <- reason:
	default:
	}
}

func (a *App) Reschedule() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}
