package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"html"
	"path"
	"sort"
	"strings"
	"time"
)

// ReleaseRecord is everything we know about one release; it feeds both the
// RSS item and the web UI.
type ReleaseRecord struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Thumbnail   string   `json:"thumbnail"`
	RSSTitle    string   `json:"rss_title"`
	Detail      Detail   `json:"detail"`
	Links       []string `json:"links"`
	PubDate     string   `json:"pub_date"` // RFC3339
	UpdatedAt   string   `json:"updated_at"`
	Description string   `json:"-"`
}

func escapeHTML(s string) string { return html.EscapeString(s) }

func buildDescription(d Detail, releaseID string, links []string) string {
	var b strings.Builder
	if d.Cover != "" {
		fmt.Fprintf(&b, `<div style="text-align:center;margin-bottom:10px;"><img src="%s" style="max-width:300px;border:1px solid #444;border-radius:4px;" /></div>`+"\n", d.Cover)
	}
	title := d.Title
	if title == "" {
		title = releaseID
	}
	fmt.Fprintf(&b, `<h3 style="margin:0;">%s — %s</h3>`+"\n", releaseID, escapeHTML(title))
	b.WriteString(`<hr style="border-color:#444;" />` + "\n")
	b.WriteString(`<table style="border-collapse:collapse;width:100%;font-size:14px;">` + "\n")
	for _, kv := range [][2]string{
		{"Actress", d.Actress}, {"Director", d.Director}, {"Duration", d.Duration},
		{"Release Date", d.ReleaseDate}, {"Product No.", releaseID},
	} {
		v := kv[1]
		if v == "" {
			v = "Unknown"
		}
		fmt.Fprintf(&b, `<tr><td style="padding:4px 8px;font-weight:bold;width:120px;color:#aaa;">%s</td><td style="padding:4px 8px;">%s</td></tr>`+"\n", kv[0], escapeHTML(v))
	}
	b.WriteString("</table>\n")
	if d.Story != "" {
		fmt.Fprintf(&b, `<div style="margin:10px 0;padding:8px;background:#1a1a1a;border-radius:4px;font-size:13px;line-height:1.6;color:#ddd;">%s</div>`+"\n", escapeHTML(d.Story))
	}
	if len(d.Screenshots) > 0 {
		b.WriteString(`<hr style="border-color:#444;margin-top:10px;" />` + "\n")
		b.WriteString(`<div style="display:flex;flex-wrap:wrap;gap:8px;margin:8px 0;">` + "\n")
		for _, s := range d.Screenshots {
			fmt.Fprintf(&b, `<a href="%s"><img src="%s" style="width:600px;height:auto;border:2px solid #444;border-radius:4px;display:block;margin:4px 0;" /></a>`+"\n", s, s)
		}
		b.WriteString("</div>\n")
	}
	if len(links) > 0 {
		b.WriteString(`<hr style="border-color:#444;margin-top:10px;" />` + "\n")
		b.WriteString(`<h4 style="margin:5px 0;">Download Links (keep2share)</h4>` + "\n")
		for _, l := range links {
			name := path.Base(strings.SplitN(l, "?", 2)[0])
			fmt.Fprintf(&b, `<p style="margin:4px 0;"><a href="%s">%s</a></p>`+"\n", l, name)
		}
	}
	return b.String()
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	Description string  `xml:"description"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate"`
}
type rssGUID struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}
type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}
type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	AtomLink      atomLink  `xml:"http://www.w3.org/2005/Atom link"`
	Docs          string    `xml:"docs"`
	Generator     string    `xml:"generator"`
	Language      string    `xml:"language"`
	LastBuildDate string    `xml:"lastBuildDate"`
	PubDate       string    `xml:"pubDate"`
	Items         []rssItem `xml:"item"`
}
type rssDoc struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Atom    string     `xml:"xmlns:atom,attr"`
	Channel rssChannel `xml:"channel"`
}

func guidFor(id, url string) string {
	h := md5.Sum([]byte(id + url))
	return hex.EncodeToString(h[:])
}

func renderRSS(records []ReleaseRecord, selfURL string) ([]byte, error) {
	sorted := append([]ReleaseRecord(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].PubDate > sorted[j].PubDate })
	now := time.Now().UTC().Format(time.RFC1123Z)
	doc := rssDoc{
		Version: "2.0",
		Atom:    "http://www.w3.org/2005/Atom",
		Channel: rssChannel{
			Title:         "Akiba-Web Heroine Collection",
			Link:          selfURL,
			Description:   "GIGA superheroine releases with metadata, screenshots (large 600px inline), and download links",
			AtomLink:      atomLink{Href: selfURL, Rel: "self"},
			Docs:          "http://www.rssboard.org/rss-specification",
			Generator:     "akiba-web-rss (go)",
			Language:      "en",
			LastBuildDate: now,
			PubDate:       now,
		},
	}
	for _, r := range sorted {
		t, err := time.Parse(time.RFC3339, r.PubDate)
		if err != nil {
			t = time.Now().UTC()
		}
		doc.Channel.Items = append(doc.Channel.Items, rssItem{
			Title:       r.RSSTitle,
			Link:        r.URL,
			Description: buildDescription(r.Detail, r.ID, r.Links),
			GUID:        rssGUID{IsPermaLink: "false", Value: guidFor(r.ID, r.URL)},
			PubDate:     t.UTC().Format(time.RFC1123Z),
		})
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
