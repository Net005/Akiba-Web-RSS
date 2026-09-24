package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MyJD is a small native client for the MyJDownloader API (port of the
// Python `myjdapi` package: AES-128-CBC + HMAC-SHA256 signed requests).
type MyJD struct {
	mu       sync.Mutex
	apiURL   string
	appKey   string
	client   *http.Client
	email    string
	password string

	loginSecret  []byte
	deviceSecret []byte
	sessionToken string
	regainToken  string
	serverEnc    []byte
	deviceEnc    []byte
	rid          int64
	connected    bool
}

type JDDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// MyJDError carries the API's error type (e.g. TOKEN_INVALID).
type MyJDError struct {
	Status int
	Type   string
	Body   string
}

func (e *MyJDError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("MyJD error %s (HTTP %d): %s", e.Type, e.Status, e.Body)
	}
	return fmt.Sprintf("MyJD HTTP %d: %s", e.Status, e.Body)
}

func NewMyJD() *MyJD {
	return &MyJD{
		apiURL: "https://api.jdownloader.org",
		appKey: "http://git.io/vmcsk",
		client: &http.Client{Timeout: 45 * time.Second},
	}
}

func sha(b ...[]byte) []byte {
	h := sha256.New()
	for _, x := range b {
		h.Write(x)
	}
	return h.Sum(nil)
}

func secretCreate(email, password, domain string) []byte {
	return sha([]byte(strings.ToLower(email) + password + domain))
}

func sign(key []byte, data string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return hex.EncodeToString(m.Sum(nil))
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("empty ciphertext")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > len(b) {
		return nil, errors.New("bad padding")
	}
	return b[:len(b)-n], nil
}

// encrypt: iv = token[:16], key = token[16:], AES-CBC, base64.
func jdEncrypt(token, plain []byte) (string, error) {
	blk, err := aes.NewCipher(token[16:])
	if err != nil {
		return "", err
	}
	buf := pkcs7Pad(append([]byte{}, plain...), aes.BlockSize)
	cipher.NewCBCEncrypter(blk, token[:16]).CryptBlocks(buf, buf)
	return base64.StdEncoding.EncodeToString(buf), nil
}

func jdDecrypt(token []byte, b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return nil, errors.New("bad ciphertext length")
	}
	blk, err := aes.NewCipher(token[16:])
	if err != nil {
		return nil, err
	}
	cipher.NewCBCDecrypter(blk, token[:16]).CryptBlocks(raw, raw)
	return pkcs7Unpad(raw)
}

func (j *MyJD) nextRID() int64 {
	now := time.Now().UnixMilli()
	if now <= j.rid {
		now = j.rid + 1
	}
	j.rid = now
	return now
}

func (j *MyJD) Connected() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.connected
}

func (j *MyJD) Connect(email, password string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.email, j.password = email, password
	j.loginSecret = secretCreate(email, password, "server")
	j.deviceSecret = secretCreate(email, password, "device")
	j.serverEnc = nil
	j.connected = false

	resp, err := j.serverGET("/my/connect", [][2]string{{"email", email}, {"appkey", j.appKey}}, j.loginSecret)
	if err != nil {
		return err
	}
	st, _ := resp["sessiontoken"].(string)
	rt, _ := resp["regaintoken"].(string)
	if st == "" {
		return errors.New("MyJD connect: no session token in response")
	}
	sb, err := hex.DecodeString(st)
	if err != nil {
		return err
	}
	j.sessionToken, j.regainToken = st, rt
	j.serverEnc = sha(j.loginSecret, sb)
	j.deviceEnc = sha(j.deviceSecret, sb)
	j.connected = true
	return nil
}

// serverGET performs a signed GET whose response is encrypted with key.
func (j *MyJD) serverGET(path string, params [][2]string, key []byte) (map[string]any, error) {
	var q strings.Builder
	q.WriteString(path + "?")
	for _, p := range params {
		q.WriteString(p[0] + "=" + url.QueryEscape(p[1]) + "&")
	}
	rid := j.nextRID()
	q.WriteString("rid=" + strconv.FormatInt(rid, 10))
	query := q.String()
	full := j.apiURL + query + "&signature=" + sign(key, query)
	resp, err := j.client.Get(full)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, apiError(resp.StatusCode, body)
	}
	plain, err := jdDecrypt(key, string(body))
	if err != nil {
		return nil, fmt.Errorf("MyJD decrypt failed: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, err
	}
	if got, ok := out["rid"].(float64); ok && int64(got) != rid {
		return nil, errors.New("MyJD: request id mismatch")
	}
	return out, nil
}

func apiError(status int, body []byte) error {
	e := &MyJDError{Status: status, Body: strings.Join(strings.Fields(string(body)), " ")}
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if t, ok := m["type"].(string); ok {
			e.Type = t
		}
	}
	if e.Type == "" && strings.Contains(strings.ToUpper(e.Body), "TOKEN_INVALID") {
		e.Type = "TOKEN_INVALID"
	}
	return e
}

func (j *MyJD) ListDevices() ([]JDDevice, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.connected {
		return nil, errors.New("MyJD not connected")
	}
	resp, err := j.serverGET("/my/listdevices", [][2]string{{"sessiontoken", j.sessionToken}}, j.serverEnc)
	if err != nil {
		return nil, err
	}
	var devs []JDDevice
	if l, ok := resp["list"].([]any); ok {
		for _, it := range l {
			m, _ := it.(map[string]any)
			d := JDDevice{}
			d.ID, _ = m["id"].(string)
			d.Name, _ = m["name"].(string)
			d.Type, _ = m["type"].(string)
			devs = append(devs, d)
		}
	}
	return devs, nil
}

// Call invokes a device action (e.g. "/linkgrabberv2/addLinks") and returns
// the "data" field of the decrypted response.
func (j *MyJD) Call(deviceID, action string, params ...any) (any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.connected {
		return nil, errors.New("MyJD not connected")
	}
	ps := make([]string, 0, len(params))
	for _, p := range params {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		ps = append(ps, string(b))
	}
	rid := j.nextRID()
	// params must be a list of JSON *strings* embedded verbatim, so build
	// the envelope by hand: strings are escaped, nothing else is quoted.
	var body bytes.Buffer
	body.WriteString(`{"apiVer":1,"url":`)
	ub, _ := json.Marshal(action)
	body.Write(ub)
	body.WriteString(`,"params":[`)
	for i, p := range ps {
		if i > 0 {
			body.WriteByte(',')
		}
		sb, _ := json.Marshal(p)
		body.Write(sb)
	}
	body.WriteString(`],"rid":` + strconv.FormatInt(rid, 10) + `}`)

	enc, err := jdEncrypt(j.deviceEnc, body.Bytes())
	if err != nil {
		return nil, err
	}
	u := j.apiURL + "/t_" + url.PathEscape(j.sessionToken) + "_" + url.PathEscape(deviceID) + action
	req, _ := http.NewRequest("POST", u, strings.NewReader(enc))
	req.Header.Set("Content-Type", "application/aesjson-jd; charset=utf-8")
	resp, err := j.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, apiError(resp.StatusCode, raw)
	}
	plain, err := jdDecrypt(j.deviceEnc, string(raw))
	if err != nil {
		return nil, fmt.Errorf("MyJD decrypt failed: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, err
	}
	return out["data"], nil
}

var trailingNum = regexp.MustCompile(`(\d+)$`)

func deviceNumber(name string) (int, bool) {
	m := trailingNum.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

func sortDevices(devs []JDDevice) {
	sort.SliceStable(devs, func(i, j int) bool {
		ni, oi := deviceNumber(devs[i].Name)
		nj, oj := deviceNumber(devs[j].Name)
		switch {
		case oi && oj:
			return ni < nj
		case oi != oj:
			return oi // numbered first
		default:
			return devs[i].Name < devs[j].Name
		}
	})
}

func isTokenInvalid(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "TOKEN_INVALID")
}

func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToUpper(err.Error())
	for _, k := range []string{"DUPLICATE", "DUPE", "ALREADY EXISTS", "ALREADY_EXISTS"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}
