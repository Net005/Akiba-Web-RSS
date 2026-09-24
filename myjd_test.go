package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCryptoRoundTrip(t *testing.T) {
	tok := sha([]byte("k"))
	enc, err := jdEncrypt(tok, []byte(`{"hello":"wörld"}`))
	if err != nil {
		t.Fatal(err)
	}
	dec, err := jdDecrypt(tok, enc)
	if err != nil || string(dec) != `{"hello":"wörld"}` {
		t.Fatalf("%q %v", dec, err)
	}
}

func TestDeviceOrdering(t *testing.T) {
	d := []JDDevice{{Name: "JD-b"}, {Name: "JD3"}, {Name: "JD1"}, {Name: "JD-a"}, {Name: "JD2"}}
	sortDevices(d)
	got := []string{}
	for _, x := range d {
		got = append(got, x.Name)
	}
	if strings.Join(got, ",") != "JD1,JD2,JD3,JD-a,JD-b" {
		t.Fatal(got)
	}
}

// A fake MyJDownloader server implementing the documented protocol, to
// verify signatures, key derivation and the device-call envelope.
func TestConnectListAndAddLinks(t *testing.T) {
	email, pass := "Me@Example.com", "pw"
	login := secretCreate(email, pass, "server")
	devSecret := secretCreate(email, pass, "device")
	session := "00112233445566778899aabbccddeeff"
	sb, _ := hex.DecodeString(session)
	serverEnc := sha(login, sb)
	deviceEnc := sha(devSecret, sb)
	var added map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/my/connect":
			q := r.URL.RawQuery
			i := strings.LastIndex(q, "&signature=")
			if sign(login, r.URL.Path+"?"+q[:i]) != q[i+len("&signature="):] {
				http.Error(w, `{"type":"AUTH_FAILED"}`, 403)
				return
			}
			rid := r.URL.Query().Get("rid")
			enc, _ := jdEncrypt(login, []byte(`{"rid":`+rid+`,"sessiontoken":"`+session+`","regaintoken":"r"}`))
			io.WriteString(w, enc)
		case r.URL.Path == "/my/listdevices":
			q := r.URL.RawQuery
			i := strings.LastIndex(q, "&signature=")
			if sign(serverEnc, r.URL.Path+"?"+q[:i]) != q[i+len("&signature="):] {
				http.Error(w, `{"type":"TOKEN_INVALID"}`, 403)
				return
			}
			rid := r.URL.Query().Get("rid")
			enc, _ := jdEncrypt(serverEnc, []byte(`{"rid":`+rid+`,"list":[{"id":"d1","name":"JD2","type":"j"},{"id":"d0","name":"JD1","type":"j"}]}`))
			io.WriteString(w, enc)
		case strings.HasPrefix(r.URL.Path, "/t_"+session+"_d1/linkgrabberv2/addLinks"):
			body, _ := io.ReadAll(r.Body)
			plain, err := jdDecrypt(deviceEnc, string(body))
			if err != nil {
				http.Error(w, "bad", 400)
				return
			}
			var env struct {
				URL    string   `json:"url"`
				Params []string `json:"params"`
				RID    int64    `json:"rid"`
			}
			if err := json.Unmarshal(plain, &env); err != nil || len(env.Params) != 1 {
				http.Error(w, "bad env", 400)
				return
			}
			_ = json.Unmarshal([]byte(env.Params[0]), &added)
			enc, _ := jdEncrypt(deviceEnc, []byte(`{"rid":`+itoa(env.RID)+`,"data":{"id":1}}`))
			io.WriteString(w, enc)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	j := NewMyJD()
	j.apiURL = srv.URL
	if err := j.Connect(email, pass); err != nil {
		t.Fatal(err)
	}
	devs, err := j.ListDevices()
	if err != nil || len(devs) != 2 {
		t.Fatalf("%v %v", devs, err)
	}
	sortDevices(devs)
	if devs[0].Name != "JD1" {
		t.Fatal("not sorted")
	}
	if _, err := j.Call("d1", "/linkgrabberv2/addLinks", map[string]any{"autostart": true, "links": "https://k2s.cc/x", "packageName": "SPSF-1"}); err != nil {
		t.Fatal(err)
	}
	if added["packageName"] != "SPSF-1" || added["autostart"] != true {
		t.Fatalf("server saw %v", added)
	}
	_ = url.QueryEscape
	_ = bytes.MinRead
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestErrorClassification(t *testing.T) {
	if !isTokenInvalid(apiError(403, []byte(`{"type":"TOKEN_INVALID"}`))) {
		t.Fatal("token")
	}
	if !isDuplicate(apiError(500, []byte(`{"type":"FILE_ALREADY_EXISTS"}`))) {
		t.Fatal("dup")
	}
}
