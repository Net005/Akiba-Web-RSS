package main

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// ForumScraper reads k2s links from the vipergirls thread.
//
// BUG FIX (Python version): when max-page discovery failed with
// "Connection reset by peer", the old code silently fell back to max_page=1
// and then scraped page 1 (the OLDEST page) — so new releases were never
// matched. Now:
//  1. every request is retried with exponential backoff + jitter on a FRESH
//     TCP connection (resets are usually per-connection rate limiting / WAF);
//  2. discovery has a second strategy (request a huge page number, vBulletin
//     clamps it to the last page) plus forward-probing from the last known page;
//  3. if discovery still fails, the last known max page from state is used;
//  4. if there is nothing to fall back to, the forum step is SKIPPED with a
//     clear error instead of scraping the wrong page.
type ForumScraper struct {
	base     string
	thread   string
	maxPages int
	retries  int
	log      *Hub
	client   *http.Client
	sleep    func(time.Duration) // injectable for tests
}

var (
	pageNumRe = regexp.MustCompile(`/page-?(\d+)`)
	releaseRe = regexp.MustCompile(`(?i)([A-Z]{2,8})-(\d{2,4})`)
)

func NewForumScraper(base, thread string, maxPages, retries int, log *Hub) *ForumScraper {
	jar, _ := cookiejar.New(nil)
	return &ForumScraper{
		base:     strings.TrimRight(base, "/"),
		thread:   strings.Trim(thread, "/"),
		maxPages: maxPages,
		retries:  retries,
		log:      log,
		client:   newForumClient(jar, false),
		sleep:    time.Sleep,
	}
}

func newForumClient(jar http.CookieJar, noKeepAlive bool) *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableKeepAlives:     noKeepAlive,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{Jar: jar, Timeout: 45 * time.Second, Transport: tr}
}

func (f *ForumScraper) pageURL(n int) string {
	return fmt.Sprintf("%s/%s/page%d", f.base, f.thread, n)
}

func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := err.Error()
	for _, m := range []string{"connection reset", "connection aborted", "broken pipe", "EOF", "timeout", "TLS handshake", "unexpected end"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// fetch GETs a URL with retry/backoff. Retries use a brand-new connection.
// Returns the body and final status code.
func (f *ForumScraper) fetch(u string) ([]byte, int, error) {
	var lastErr error
	for attempt := 1; attempt <= f.retries; attempt++ {
		client := f.client
		if attempt > 1 {
			// Fresh connection (and no keep-alive) after a failure.
			client = newForumClient(f.client.Jar, true)
			f.client.CloseIdleConnections()
		}
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", browserUA)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Referer", f.base+"/")
		resp, err := client.Do(req)
		if err == nil {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil {
				if resp.StatusCode == 429 || resp.StatusCode >= 500 {
					lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				} else {
					return body, resp.StatusCode, nil
				}
			} else {
				err = rerr
			}
		}
		if err != nil {
			lastErr = err
			if !isTransient(err) {
				return nil, 0, err
			}
		}
		if attempt < f.retries {
			delay := time.Duration(1<<uint(attempt)) * time.Second // 2s,4s,8s...
			delay += time.Duration(rand.Intn(1000)) * time.Millisecond
			f.log.Warn("Forum request failed (attempt %d/%d): %v — retrying in %.1fs", attempt, f.retries, lastErr, delay.Seconds())
			f.sleep(delay)
		}
	}
	return nil, 0, fmt.Errorf("after %d attempts: %w", f.retries, lastErr)
}

func maxPageFromHTML(body []byte) int {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return 0
	}
	max := 0
	doc.Find("span.selected, span.curr, a.selected").Each(func(_ int, s *goquery.Selection) {
		if n, err := strconv.Atoi(strings.TrimSpace(s.Text())); err == nil && n > max {
			max = n
		}
	})
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if m := pageNumRe.FindStringSubmatch(href); m != nil {
			if n, _ := strconv.Atoi(m[1]); n > max {
				max = n
			}
		}
	})
	return max
}

// DiscoverMaxPage returns the thread's last page. lastKnown (0 = none) is
// used for forward probing and as the fallback.
func (f *ForumScraper) DiscoverMaxPage(lastKnown int) (int, error) {
	// Strategy 1: thread front page pagination.
	body, code, err := f.fetch(fmt.Sprintf("%s/%s", f.base, f.thread))
	if err == nil && code == 200 {
		if n := maxPageFromHTML(body); n > 0 {
			if lastKnown > n {
				n = lastKnown // pagination widget only showed a window
			}
			return f.probeForward(n), nil
		}
	} else if err != nil {
		f.log.Warn("Forum max page discovery (front page) failed: %v", err)
	}

	// Strategy 2: ask for an absurd page number; the forum clamps to the last one.
	body, code, err2 := f.fetch(f.pageURL(99999))
	if err2 == nil && code == 200 {
		if n := maxPageFromHTML(body); n > 0 {
			return f.probeForward(maxInt(n, lastKnown)), nil
		}
	}

	// Strategy 3: last known page, probing forward for growth.
	if lastKnown > 0 {
		f.log.Warn("Forum max page discovery failed; using last known page %d", lastKnown)
		return f.probeForward(lastKnown), nil
	}
	if err == nil {
		err = err2
	}
	if err == nil {
		err = errors.New("no pagination found")
	}
	return 0, fmt.Errorf("forum max page discovery failed: %w", err)
}

// probeForward checks whether page n+1, n+2... exist (thread grew).
func (f *ForumScraper) probeForward(n int) int {
	for i := 0; i < 3; i++ {
		body, code, err := f.fetchOnce(f.pageURL(n + 1))
		if err != nil || code != 200 {
			break
		}
		// vBulletin serves the last page for out-of-range numbers; only
		// accept it if the page's own pagination says it is that page.
		if maxPageFromHTML(body) >= n+1 && pageIsSelected(body, n+1) {
			n++
			continue
		}
		break
	}
	return n
}

func pageIsSelected(body []byte, n int) bool {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return false
	}
	ok := false
	doc.Find("span.selected, span.curr, a.selected").Each(func(_ int, s *goquery.Selection) {
		if v, err := strconv.Atoi(strings.TrimSpace(s.Text())); err == nil && v == n {
			ok = true
		}
	})
	return ok
}

// fetchOnce is fetch without retries (used for cheap probes).
func (f *ForumScraper) fetchOnce(u string) ([]byte, int, error) {
	saved := f.retries
	f.retries = 1
	defer func() { f.retries = saved }()
	return f.fetch(u)
}

// FetchLinks scrapes the last maxPages pages. It returns links by release ID,
// the discovered max page, and an error if the whole step could not run.
func (f *ForumScraper) FetchLinks(lastKnown int) (map[string][]string, int, error) {
	links := map[string][]string{}
	maxPage, err := f.DiscoverMaxPage(lastKnown)
	if err != nil {
		return links, 0, err
	}
	start := maxInt(maxPage-f.maxPages+1, 1)
	f.log.Info("Forum thread max page=%d, scraping pages %d-%d", maxPage, start, maxPage)

	okPages := 0
	for p := start; p <= maxPage; p++ {
		body, code, err := f.fetch(f.pageURL(p))
		if err != nil {
			f.log.Error("Forum page %d failed: %v", p, err)
			continue
		}
		if code != 200 {
			f.log.Error("Forum page %d failed: HTTP %d", p, code)
			continue
		}
		okPages++
		found := parseForumLinks(body, links)
		f.log.Info("Forum page %d: found links for %d release IDs", p, found)
	}
	if okPages == 0 {
		return links, maxPage, errors.New("no forum pages could be fetched")
	}
	f.log.Info("Forum: found k2s links for %d release IDs", len(links))
	return links, maxPage, nil
}

func parseForumLinks(body []byte, out map[string][]string) int {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return 0
	}
	ids := map[string]bool{}
	doc.Find("a[href*='k2s.cc'], a[href*='k2s.to']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		combined := strings.TrimSpace(s.Text()) + " " + href
		m := releaseRe.FindStringSubmatch(combined)
		if m == nil {
			return
		}
		id := strings.ToUpper(m[1]) + "-" + m[2]
		ids[id] = true
		for _, e := range out[id] {
			if e == href {
				return
			}
		}
		out[id] = append(out[id], href)
	})
	return len(ids)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
