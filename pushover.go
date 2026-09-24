package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const pushoverURL = "https://api.pushover.net/1/messages.json"

func truncUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

func destID(d PushoverDest) string {
	h := sha256.Sum256([]byte(d.APIToken + "\x00" + d.UserKey))
	return hex.EncodeToString(h[:])
}

type PushoverSender struct {
	client *http.Client
	url    string
}

func NewPushoverSender() *PushoverSender {
	return &PushoverSender{client: &http.Client{Timeout: 30 * time.Second}, url: pushoverURL}
}

// Send posts one message. If d.IncludeSensitiveData is set, rich content is
// sent (cover attachment fetched once and cached via *cover).
func (p *PushoverSender) Send(d PushoverDest, title, story, coverURL, releaseURL string, cover **coverBlob) error {
	fields := map[string]string{"token": d.APIToken, "user": d.UserKey, "message": "New AW release found"}
	var attach *coverBlob
	if d.IncludeSensitiveData {
		if title == "" || coverURL == "" || releaseURL == "" {
			return errors.New("release title, cover, or URL is unavailable")
		}
		if *cover == nil {
			r, err := p.client.Get(coverURL)
			if err != nil {
				return err
			}
			b, err := io.ReadAll(io.LimitReader(r.Body, 5*1024*1024+1))
			r.Body.Close()
			if err != nil {
				return err
			}
			if r.StatusCode >= 400 {
				return fmt.Errorf("cover HTTP %d", r.StatusCode)
			}
			if len(b) > 5*1024*1024 {
				return errors.New("release cover exceeds Pushover's 5 MiB limit")
			}
			ct := strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]
			if ct == "" {
				ct = "image/jpeg"
			}
			if !strings.HasPrefix(ct, "image/") {
				return errors.New("release cover response is not an image")
			}
			*cover = &coverBlob{data: b, ctype: ct}
		}
		attach = *cover
		fields["title"] = truncUTF8(title, 250)
		msg := strings.TrimSpace(story)
		if msg == "" {
			msg = "Story unavailable"
		}
		fields["message"] = truncUTF8(msg, 1024)
		fields["url"] = releaseURL
		fields["url_title"] = "View on Akiba-Web"
	}

	var body io.Reader
	ctype := ""
	if attach != nil {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range fields {
			_ = mw.WriteField(k, v)
		}
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="attachment"; filename="release-cover"`}
		h["Content-Type"] = []string{attach.ctype}
		pw, _ := mw.CreatePart(h)
		pw.Write(attach.data)
		mw.Close()
		body, ctype = &buf, mw.FormDataContentType()
	} else {
		v := url.Values{}
		for k, val := range fields {
			v.Set(k, val)
		}
		body, ctype = strings.NewReader(v.Encode()), "application/x-www-form-urlencoded"
	}
	req, _ := http.NewRequest("POST", p.url, body)
	req.Header.Set("Content-Type", ctype)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		Status int `json:"status"`
		Errors any `json:"errors"`
	}
	_ = json.Unmarshal(raw, &res)
	if resp.StatusCode >= 400 || res.Status != 1 {
		return fmt.Errorf("pushover: HTTP %d %v", resp.StatusCode, res.Errors)
	}
	return nil
}

type coverBlob struct {
	data  []byte
	ctype string
}

// SendRaw sends a plain message (used for the UI's "test" button).
func (p *PushoverSender) SendRaw(d PushoverDest, message string) error {
	v := url.Values{"token": {d.APIToken}, "user": {d.UserKey}, "message": {message}}
	resp, err := p.client.PostForm(p.url, v)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		Status int `json:"status"`
		Errors any `json:"errors"`
	}
	_ = json.Unmarshal(raw, &res)
	if resp.StatusCode >= 400 || res.Status != 1 {
		return fmt.Errorf("pushover: HTTP %d %v", resp.StatusCode, res.Errors)
	}
	return nil
}
