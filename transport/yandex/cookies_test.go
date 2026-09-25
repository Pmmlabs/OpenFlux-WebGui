package yandex

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistJarSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")

	p1 := newPersistJar(path)
	u := mustURL(t, "https://docs.yandex.ru/edit")
	p1.SetCookies(u, []*http.Cookie{
		{Name: "session", Value: "abc", Domain: "yandex.ru", Path: "/"},
		{Name: "uid", Value: "42", Domain: ".yandex.ru", Path: "/", Expires: time.Now().Add(time.Hour)},
	})
	p1.Save()

	// Fresh jar from the same file must see the same cookies.
	p2 := newPersistJar(path)
	got := p2.Cookies(u)
	if len(got) != 2 {
		t.Fatalf("expected 2 cookies after reload, got %d", len(got))
	}
	byName := map[string]string{}
	for _, c := range got {
		byName[c.Name] = c.Value
	}
	if byName["session"] != "abc" || byName["uid"] != "42" {
		t.Errorf("cookie values lost across reload: %v", byName)
	}
}

func TestPersistJarSkipsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")

	p1 := newPersistJar(path)
	u := mustURL(t, "https://docs.yandex.ru/")
	p1.SetCookies(u, []*http.Cookie{
		{Name: "old", Value: "x", Expires: time.Now().Add(-time.Minute)},
		{Name: "fresh", Value: "y", Expires: time.Now().Add(time.Hour)},
	})
	p1.Save()

	p2 := newPersistJar(path)
	got := p2.Cookies(u)
	if len(got) != 1 || got[0].Name != "fresh" {
		t.Fatalf("expected only the fresh cookie, got %+v", got)
	}
}

func TestPersistJarClearDropsStateAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")

	p := newPersistJar(path)
	u := mustURL(t, "https://docs.yandex.ru/")
	p.SetCookies(u, []*http.Cookie{{Name: "session", Value: "abc"}})
	p.Save()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cookie file should exist after Save: %v", err)
	}

	p.Clear()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("cookie file should be removed after Clear")
	}
	if got := p.Cookies(u); len(got) != 0 {
		t.Errorf("in-memory cookies should be dropped after Clear, got %d", len(got))
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
