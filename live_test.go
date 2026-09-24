package main

import (
	"os"
	"testing"
)

// Live smoke test against the real site: LIVE=1 go test -run TestLiveAkiba -v
func TestLiveAkiba(t *testing.T) {
	if os.Getenv("LIVE") == "" {
		t.Skip("set LIVE=1")
	}
	h := NewHub(100)
	a := NewAkibaScraper("https://www.akiba-web.com", h)
	rel, err := a.FetchReleases("/search/?narrow=2&sort=1")
	if err != nil || len(rel) == 0 {
		t.Fatalf("%v %d", err, len(rel))
	}
	t.Logf("%d releases, first %+v", len(rel), rel[0])
	d, err := a.FetchDetail(rel[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", d)
}

func TestLiveForum(t *testing.T) {
	if os.Getenv("LIVE") == "" {
		t.Skip("set LIVE=1")
	}
	f := NewForumScraper("https://vipergirls.to", "threads/5913172-Heroine-superheroine-JAV-movie-collection", 1, 3, NewHub(100))
	links, max, err := f.FetchLinks(0)
	t.Logf("max=%d ids=%d err=%v", max, len(links), err)
}
