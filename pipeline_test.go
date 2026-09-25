package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRSSRendersNewestFirstAndValidXML(t *testing.T) {
	recs := []ReleaseRecord{
		{ID: "A-1", URL: "http://x/1", RSSTitle: "[A-1] old & <b>", PubDate: "2026-01-01T00:00:00Z", Detail: Detail{Title: "old", Story: "a<b"}},
		{ID: "B-2", URL: "http://x/2", RSSTitle: "[B-2] new", PubDate: "2026-06-01T00:00:00Z", Links: []string{"https://k2s.cc/file/1/B-2_01.rar?site=x"}},
	}
	b, err := renderRSS(recs, "http://localhost:5000/")
	if err != nil {
		t.Fatal(err)
	}
	var d rssDoc
	if err := xml.Unmarshal(b, &d); err != nil {
		t.Fatalf("invalid xml: %v", err)
	}
	if len(d.Channel.Items) != 2 || !strings.Contains(d.Channel.Items[0].Title, "B-2") {
		t.Fatalf("bad order: %+v", d.Channel.Items)
	}
	if !strings.Contains(d.Channel.Items[0].Description, "B-2_01.rar") {
		t.Fatal("link missing from description")
	}
}

func TestStateMigratesLegacyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(p, []byte(`{"found_links":{"https://k2s.cc/file/x/SPSF-52_01.FHD.rar":true},"queued_releases":{"SPSF-52":true}}`), 0o600)
	s := NewStateStore(p)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if s.S.QueuedReleases["SPSF-52"] == "" || s.S.NotifiedReleases == nil || s.S.PendingNotifications == nil {
		t.Fatalf("%+v", s.S)
	}
}

func TestConfigPreservesUnknownKeysAndSecrets(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"myjd_email":"a@b.c","myjd_password":"secret","custom":1,"port":5000}`), 0o600)
	cs, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Update(map[string]any{"myjd_password": secretMask, "port": 6000}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &m)
	if m["myjd_password"] != "secret" || m["custom"] != float64(1) || m["port"] != float64(6000) {
		t.Fatalf("%v", m)
	}
	if cs.Redacted()["myjd_password"] != secretMask {
		t.Fatal("not redacted")
	}
}

// End-to-end: fake Akiba + fake flaky forum → feed written, forum bug fixed.
func TestPipelineEndToEnd(t *testing.T) {
	var forumFront int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/search/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div class="search_sam_box"><div class="pac_thum_box"><img src="/t.jpg"></div>
		<a href="/product/index.php?product_id=1"><span>Heroine Title</span></a> （SPSF-99） Release Day: 2026/09/20</div>
		<div class="search_sam_box"><a href="/product/index.php?product_id=2"><span>Older</span></a> （SPSF-98） Release Day: 2026/08/01</div>`)
	})
	mux.HandleFunc("/product/index.php", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div id="works_pic"><h5>Heroine Title Full</h5><img src="/cover.jpg"></div>
		<div id="works_txt"><dd class="yaku"><a>Actress One</a></dd><dl><dt>Director</dt><dd><a>Dir</a></dd><dt>Time</dt><dd>120min</dd><dt>Release Date</dt><dd>2026/09/20</dd></dl></div>
		<div id="sample_list"><a href="/s1_l.jpg">x</a></div><div id="story_list2"><div class="story_window">A story.</div></div>`)
	})
	akiba := httptest.NewServer(mux)
	defer akiba.Close()

	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/threads/t" && atomic.AddInt32(&forumFront, 1) == 1 {
			killConn(w) // the reported "connection reset by peer"
			return
		}
		var p int
		if _, err := fmt.Sscanf(r.URL.Path, "/threads/t/page%d", &p); err != nil || p > 3 {
			p = 3
		}
		if r.URL.Path == "/threads/t" {
			p = 1
		}
		fmt.Fprint(w, pagerHTML(p, 3)+`<a href="https://k2s.cc/file/z/SPSF-99_01.rar">SPSF-99_01.rar</a>`)
	}))
	defer forum.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "settings.json")
	b, _ := json.Marshal(map[string]any{
		"akiba_base": akiba.URL, "forum_base": forum.URL, "forum_thread": "threads/t", "forum_pages": 2,
		"forum_retries": 3, "port": 0,
	})
	os.WriteFile(cfgPath, b, 0o600)
	cs, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := NewHub(500)
	log.out = &discard{}
	app := NewApp(cs, log)
	done := make(chan bool)
	go func() { app.RunPipeline("test"); done <- true }()
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("pipeline timeout")
	}
	st := app.GetStatus()
	if st.LastRun == nil || !st.LastRun.OK {
		t.Fatalf("run failed: %+v", st.LastRun)
	}
	if !st.LastRun.ForumOK || st.LastRun.ForumPage != 3 {
		t.Fatalf("forum bug not fixed: %+v", st.LastRun)
	}
	feed, err := os.ReadFile(cs.Get().RSSFile)
	if err != nil || !strings.Contains(string(feed), "SPSF-99") || !strings.Contains(string(feed), "SPSF-99_01.rar") {
		t.Fatalf("feed wrong: %v\n%s", err, feed)
	}
	if st.LastRun.Matched != 1 {
		t.Fatalf("matched=%d", st.LastRun.Matched)
	}
	rv := app.Releases()
	if len(rv) != 2 || rv[0].ID != "SPSF-99" || rv[0].State != "waiting" {
		t.Fatalf("%+v", rv)
	}
	found := false
	for _, r := range rv {
		if r.Thumbnail != "" {
			found = true
			if !strings.HasPrefix(r.Thumbnail, "/covers/") {
				t.Fatalf("thumbnail not cached for %s: %q", r.ID, r.Thumbnail)
			}
		}
	}
	if !found {
		t.Fatal("expected at least one cached thumbnail")
	}
}

// A release that scrolls off Akiba-Web's listing page must stay in the
// archive (and thus the feed/UI) instead of being forgotten on the next run.
func TestArchivedReleasesSurviveWhenTheyLeaveTheListing(t *testing.T) {
	listingPath := int32(2) // start by serving both releases
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/search/", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&listingPath) == 2 {
			fmt.Fprint(w, `<div class="search_sam_box"><a href="/product/index.php?product_id=1"><span>New</span></a> （SPSF-99） Release Day: 2026/09/20</div>
			<div class="search_sam_box"><a href="/product/index.php?product_id=2"><span>Old</span></a> （SPSF-98） Release Day: 2026/08/01</div>`)
			return
		}
		fmt.Fprint(w, `<div class="search_sam_box"><a href="/product/index.php?product_id=1"><span>New</span></a> （SPSF-99） Release Day: 2026/09/20</div>`)
	})
	mux.HandleFunc("/product/index.php", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div id="works_pic"><h5>Title</h5><img src="/cover.jpg"></div><div id="works_txt"></div>`)
	})
	akiba := httptest.NewServer(mux)
	defer akiba.Close()
	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, pagerHTML(1, 1))
	}))
	defer forum.Close()

	dir := t.TempDir()
	b, _ := json.Marshal(map[string]any{
		"akiba_base": akiba.URL, "forum_base": forum.URL, "forum_thread": "threads/t", "forum_pages": 1,
		"forum_retries": 2, "port": 0,
	})
	os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o600)
	cs, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := NewHub(500)
	log.out = &discard{}
	app := NewApp(cs, log)

	if !app.RunPipeline("first") {
		t.Fatal("first run did not start")
	}
	if rv := app.Releases(); len(rv) != 2 {
		t.Fatalf("expected 2 releases after first run, got %d: %+v", len(rv), rv)
	}

	atomic.StoreInt32(&listingPath, 1) // SPSF-98 drops off the listing
	if !app.RunPipeline("second") {
		t.Fatal("second run did not start")
	}
	rv := app.Releases()
	if len(rv) != 2 {
		t.Fatalf("archived release was dropped: expected 2, got %d: %+v", len(rv), rv)
	}
	ids := map[string]bool{}
	for _, r := range rv {
		ids[r.ID] = true
	}
	if !ids["SPSF-98"] || !ids["SPSF-99"] {
		t.Fatalf("missing archived release: %+v", rv)
	}
}
