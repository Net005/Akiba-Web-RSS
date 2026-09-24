package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func killConn(w http.ResponseWriter) {
	hj, _ := w.(http.Hijacker)
	c, _, _ := hj.Hijack()
	c.Close() // abrupt close → client sees EOF / connection reset
}

func pagerHTML(current, last int) string {
	s := "<html><body><div class='pagenav'>"
	for i := 1; i <= last; i++ {
		if i == current {
			s += fmt.Sprintf("<span class='selected'>%d</span>", i)
		} else {
			s += fmt.Sprintf("<a href='/threads/t/page%d'>%d</a>", i, i)
		}
	}
	s += "</div>" +
		fmt.Sprintf("<a href='https://k2s.cc/file/abc/SPSF-%02d_01.rar'>x</a>", current) +
		"</body></html>"
	return s
}

func newTestForum(base string, retries int) *ForumScraper {
	f := NewForumScraper(base, "threads/t", 2, retries, NewHub(100))
	f.log.out = &discard{}
	f.sleep = func(time.Duration) {}
	return f
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// The reported bug: the first requests are reset by the peer. The scraper
// must retry and still find the REAL last page (not fall back to page 1).
func TestDiscoverMaxPageRetriesAfterConnectionReset(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/threads/t" {
			if atomic.AddInt32(&n, 1) <= 2 {
				killConn(w)
				return
			}
			fmt.Fprint(w, pagerHTML(1, 7))
			return
		}
		// page8 probe: the forum clamps to the last page (7 selected)
		fmt.Fprint(w, pagerHTML(7, 7))
	}))
	defer srv.Close()
	f := newTestForum(srv.URL, 4)
	got, err := f.DiscoverMaxPage(0)
	if err != nil || got != 7 {
		t.Fatalf("got %d, %v; want 7", got, err)
	}
	if n < 3 {
		t.Fatalf("expected retries, saw %d requests", n)
	}
}

func TestDiscoverFallsBackToLastKnownPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { killConn(w) }))
	defer srv.Close()
	f := newTestForum(srv.URL, 2)
	got, err := f.DiscoverMaxPage(42)
	if err != nil || got != 42 {
		t.Fatalf("got %d, %v; want last known 42", got, err)
	}
}

func TestNeverScrapesPageOneWhenDiscoveryFails(t *testing.T) {
	var pageReqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pageReqs, 1)
		killConn(w)
	}))
	defer srv.Close()
	f := newTestForum(srv.URL, 2)
	links, _, err := f.FetchLinks(0)
	if err == nil {
		t.Fatal("expected an error when nothing is known and discovery fails")
	}
	if len(links) != 0 {
		t.Fatal("must not return links from a guessed page")
	}
}

func TestFetchLinksScrapesNewestPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p int
		if _, err := fmt.Sscanf(r.URL.Path, "/threads/t/page%d", &p); err == nil {
			if p > 5 {
				p = 5
			}
			fmt.Fprint(w, pagerHTML(p, 5))
			return
		}
		fmt.Fprint(w, pagerHTML(1, 5))
	}))
	defer srv.Close()
	f := newTestForum(srv.URL, 2)
	links, max, err := f.FetchLinks(0)
	if err != nil || max != 5 {
		t.Fatalf("max=%d err=%v", max, err)
	}
	if _, ok := links["SPSF-05"]; !ok {
		t.Fatalf("newest page not scraped: %v", links)
	}
	if _, ok := links["SPSF-04"]; !ok {
		t.Fatalf("second newest page not scraped: %v", links)
	}
	if _, ok := links["SPSF-01"]; ok {
		t.Fatalf("scraped an old page: %v", links)
	}
}
