package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Release is one card on the Akiba-Web listing.
type Release struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	URL             string `json:"url"`
	Thumbnail       string `json:"thumbnail"`
	ListingReleased string `json:"listing_release_date"`
}

// Detail is the scraped product page.
type Detail struct {
	Title       string   `json:"title"`
	Actress     string   `json:"actress"`
	Director    string   `json:"director"`
	Duration    string   `json:"duration"`
	ReleaseDate string   `json:"release_date"`
	Cover       string   `json:"cover"`
	Screenshots []string `json:"screenshots"`
	Story       string   `json:"story"`
}

type AkibaScraper struct {
	base    string
	client  *http.Client
	log     *Hub
	timeout time.Duration
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

func NewAkibaScraper(base string, log *Hub) *AkibaScraper {
	jar, _ := cookiejar.New(nil)
	a := &AkibaScraper{
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Jar: jar, Timeout: 30 * time.Second},
		log:    log,
	}
	a.primeSession()
	return a
}

func (a *AkibaScraper) get(u, referer string, noRedirect bool) (*http.Response, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	c := a.client
	if noRedirect {
		cc := *a.client
		cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		c = &cc
	}
	return c.Do(req)
}

func (a *AkibaScraper) join(p string) string {
	u, err := url.Parse(a.base + "/")
	if err != nil {
		return p
	}
	r, err := u.Parse(p)
	if err != nil {
		return p
	}
	return r.String()
}

// primeSession enters the age gate and establishes session cookies.
func (a *AkibaScraper) primeSession() {
	if r, err := a.get(a.base, "", false); err == nil {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	} else {
		a.log.Error("Akiba session prime failed: %v", err)
		return
	}
	if r, err := a.get(a.join("cookie_set.php"), "", true); err == nil {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	r, err := a.get(a.join("top.php"), "", false)
	if err != nil {
		a.log.Error("Akiba session prime failed: %v", err)
		return
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode >= 400 {
		a.log.Error("Akiba session prime failed: HTTP %d", r.StatusCode)
		return
	}
	a.log.Info("Akiba session primed: %d", r.StatusCode)
}

var (
	relIDRe   = regexp.MustCompile(`(?i)\b([A-Z]{2,8})\s*[-‐‑‒–—]\s*(\d{1,4})\b`)
	relDateRe = regexp.MustCompile(`(?i)(?:realease|release)\s*(?:day|date)?[^0-9]*(\d{4}[-/]\d{1,2}[-/]\d{1,2})`)
)

func (a *AkibaScraper) FetchReleases(path string) ([]Release, error) {
	if path == "" {
		path = "/search/?narrow=2&sort=1"
	}
	listing := a.join(strings.TrimLeft(path, "/"))
	pu, err := url.Parse(listing)
	if err != nil {
		return nil, err
	}
	q := pu.Query()
	q.Set("sort", "1")
	pu.RawQuery = q.Encode()

	resp, err := a.get(pu.String(), a.base, false)
	if err != nil {
		return nil, fmt.Errorf("listing fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("listing fetch failed: HTTP %d", resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}
	cards := doc.Find(".search_sam_box")
	if cards.Length() == 0 {
		return nil, fmt.Errorf("failed to locate .search_sam_box on release listing")
	}
	var out []Release
	seen := map[string]bool{}
	cards.Each(func(_ int, card *goquery.Selection) {
		item := card.Find("a[href*='product_id']").First()
		if item.Length() == 0 {
			return
		}
		href, _ := item.Attr("href")
		full := a.join(href)
		if seen[full] {
			return
		}
		text := strings.Join(strings.Fields(card.Text()), " ")
		m := relIDRe.FindStringSubmatch(text)
		if m == nil {
			return
		}
		id := strings.ToUpper(m[1]) + "-" + m[2]
		title := strings.Join(strings.Fields(card.Find("a[href*='product_id'] span").First().Text()), " ")
		img := card.Find(".pac_thum_box img").First()
		if img.Length() == 0 {
			img = card.Find("img").First()
		}
		thumb := ""
		if src, ok := img.Attr("src"); ok {
			thumb = a.join(src)
		}
		date := ""
		if dm := relDateRe.FindStringSubmatch(text); dm != nil {
			date = strings.ReplaceAll(dm[1], "-", "/")
		}
		out = append(out, Release{ID: id, Title: title, URL: full, Thumbnail: thumb, ListingReleased: date})
		seen[full] = true
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].ListingReleased > out[j].ListingReleased })
	a.log.Info("Total releases found: %d", len(out))
	return out, nil
}

func ddAfter(doc *goquery.Document, label string) *goquery.Selection {
	var res *goquery.Selection
	doc.Find("#works_txt dt").EachWithBreak(func(_ int, dt *goquery.Selection) bool {
		if strings.Contains(dt.Text(), label) {
			res = dt.NextFiltered("dd")
			return false
		}
		return true
	})
	return res
}

func (a *AkibaScraper) FetchDetail(productURL string) (*Detail, error) {
	resp, err := a.get(productURL, a.base, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}
	d := &Detail{}
	title := doc.Find("#works_pic h5").First()
	if title.Length() == 0 {
		title = doc.Find("#works_txt b").First()
	}
	if title.Length() == 0 {
		title = doc.Find("h1").First()
	}
	d.Title = strings.TrimSpace(title.Text())

	var actresses []string
	doc.Find("#works_txt dd.yaku a").Each(func(_ int, s *goquery.Selection) {
		if t := strings.TrimSpace(s.Text()); t != "" {
			actresses = append(actresses, t)
		}
	})
	if len(actresses) == 0 {
		if t := strings.TrimSpace(doc.Find("#works_txt span.yaku").First().Text()); t != "" {
			actresses = []string{t}
		}
	}
	d.Actress = "Unknown"
	if len(actresses) > 0 {
		d.Actress = strings.Join(actresses, ", ")
	}

	d.Director, d.Duration, d.ReleaseDate = "Unknown", "Unknown", "Unknown"
	if dd := ddAfter(doc, "Director"); dd != nil && dd.Length() > 0 {
		if a := dd.Find("a").First(); a.Length() > 0 {
			d.Director = strings.TrimSpace(a.Text())
		} else if t := strings.TrimSpace(dd.Text()); t != "" {
			d.Director = t
		}
	}
	if dd := ddAfter(doc, "Time"); dd != nil && dd.Length() > 0 {
		if t := strings.TrimSpace(dd.Text()); t != "" {
			d.Duration = t
		}
	}
	if dd := ddAfter(doc, "Release Date"); dd != nil && dd.Length() > 0 {
		if t := strings.TrimSpace(dd.Text()); t != "" {
			d.ReleaseDate = t
		}
	}
	if src, ok := doc.Find("#works_pic img").First().Attr("src"); ok && src != "" {
		if strings.HasPrefix(src, "http") {
			d.Cover = src
		} else {
			d.Cover = a.join(src)
		}
	}
	seen := map[string]bool{}
	doc.Find("#sample_list a[href], .gasatsu_images_pc a[href]").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if strings.Contains(href, "_l.jpg") {
			href = a.join(href)
			if !seen[href] {
				seen[href] = true
				d.Screenshots = append(d.Screenshots, href)
			}
		}
	})
	if s := doc.Find("#story_list2 .story_window").First(); s.Length() > 0 {
		d.Story = strings.TrimSpace(s.Text())
	} else if s := doc.Find("#story_list1 .story_window").First(); s.Length() > 0 {
		d.Story = strings.TrimSpace(s.Text())
	}
	a.log.Info("Product detail: %s — %d screenshots, actress=%s", truncRunes(d.Title, 30), len(d.Screenshots), d.Actress)
	return d, nil
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
