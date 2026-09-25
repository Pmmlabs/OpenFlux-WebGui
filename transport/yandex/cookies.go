package yandex

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sync"
	"time"
)

// persistJar wraps a cookiejar.Jar and records every SetCookies call so the
// full cookie state (with domain/path/expiry attributes) can be serialized to
// disk and replayed on the next start.
//
// Motivation: Yandex escalates to the unsolvable silhouette captcha
// (form-fb-hint=7.73) after roughly 40 captcha solves from one IP. Every
// transport reconnect re-solved the captcha because fetchDocInfo built a
// fresh jar each time. Reusing a trusted cookie session across reconnects
// (and across process restarts, via the cookie file) keeps the solve count
// at ~one per IP.
type persistJar struct {
	mu      sync.Mutex
	inner   http.CookieJar
	path    string
	records map[string][]*http.Cookie // key: URL the cookies were set for
}

type jarFile struct {
	Cookies map[string][]cookieRecord `json:"cookies"`
}

type cookieRecord struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Domain   string    `json:"domain,omitempty"`
	Path     string    `json:"path,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HttpOnly bool      `json:"http_only,omitempty"`
}

func newPersistJar(path string) *persistJar {
	p := &persistJar{
		inner:   mustJar(),
		path:    path,
		records: make(map[string][]*http.Cookie),
	}
	if path != "" {
		p.load()
	}
	return p
}

func mustJar() http.CookieJar {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return jar
}

func (p *persistJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inner.SetCookies(u, cookies)
	if len(cookies) == 0 {
		return
	}
	key := u.String()
	existing := p.records[key]
	for _, c := range cookies {
		if c.Value == "" { // deletion
			continue
		}
		replaced := false
		for i, e := range existing {
			if e.Name == c.Name && e.Domain == c.Domain && e.Path == c.Path {
				existing[i] = c
				replaced = true
				break
			}
		}
		if !replaced {
			existing = append(existing, c)
		}
	}
	p.records[key] = existing
}

func (p *persistJar) Cookies(u *url.URL) []*http.Cookie {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inner.Cookies(u)
}

// Save writes the recorded cookie state to disk. Called after a successful
// doc fetch so the captcha-passed session survives process restarts.
func (p *persistJar) Save() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.path == "" || len(p.records) == 0 {
		return
	}
	out := jarFile{Cookies: make(map[string][]cookieRecord, len(p.records))}
	for key, cookies := range p.records {
		recs := make([]cookieRecord, 0, len(cookies))
		for _, c := range cookies {
			recs = append(recs, cookieRecord{
				Name:     c.Name,
				Value:    c.Value,
				Domain:   c.Domain,
				Path:     c.Path,
				Expires:  c.Expires,
				Secure:   c.Secure,
				HttpOnly: c.HttpOnly,
			})
		}
		out.Cookies[key] = recs
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.path)
}

// Clear drops all cookie state, in memory and on disk. Used when Yandex
// escalates to the silhouette captcha: the session is bot-flagged, so its
// cookies are poisoned and must not be reused.
func (p *persistJar) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inner = mustJar()
	p.records = make(map[string][]*http.Cookie)
	if p.path != "" {
		_ = os.Remove(p.path)
	}
}

func (p *persistJar) load() {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	// An empty file is a fresh session (the path probe creates it): keep
	// it, there is simply nothing to load yet.
	if len(data) == 0 {
		return
	}
	var f jarFile
	if err := json.Unmarshal(data, &f); err != nil {
		_ = os.Remove(p.path)
		return
	}
	for key, recs := range f.Cookies {
		u, err := url.Parse(key)
		if err != nil {
			continue
		}
		cookies := make([]*http.Cookie, 0, len(recs))
		for _, r := range recs {
			// Zero Expires = session cookie: keep it (it only lives until
			// the process exits anyway, but must survive the reload itself).
			if !r.Expires.IsZero() && r.Expires.Before(time.Now()) {
				continue
			}
			cookies = append(cookies, &http.Cookie{
				Name:     r.Name,
				Value:    r.Value,
				Domain:   r.Domain,
				Path:     r.Path,
				Expires:  r.Expires,
				Secure:   r.Secure,
				HttpOnly: r.HttpOnly,
			})
		}
		if len(cookies) > 0 {
			p.inner.SetCookies(u, cookies)
			p.records[key] = cookies
		}
	}
}
