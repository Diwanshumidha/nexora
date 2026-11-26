package updates

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.0", true}, {"v1.0.0", "0.9.9", true}, {"0.1.10", "0.1.9", true},
		{"0.1.0", "0.1.0", false}, {"0.1.0", "0.2.0", false}, {"0.2.0-rc1", "0.2.0", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestCheck(t *testing.T) {
	body := `{"tag_name":"v0.3.0","html_url":"https://github.com/x/y/releases/tag/v0.3.0","body":"Fixes"}`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "Nexora/0.1.0" {
			t.Errorf("user agent %q", r.Header.Get("User-Agent"))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL, "0.1.0", slog.New(slog.NewTextHandler(io.Discard, nil)))

	info := c.Check(context.Background())
	if !info.Available || info.Latest != "0.3.0" || info.URL == "" || info.Notes != "Fixes" {
		t.Fatalf("%+v", info)
	}
	// A failed check keeps the last known release.
	status = http.StatusBadGateway
	info = c.Check(context.Background())
	if !info.Available || info.Latest != "0.3.0" || info.Error == "" {
		t.Fatalf("after failure: %+v", info)
	}
	// Up to date.
	status, body = http.StatusOK, `{"tag_name":"v0.1.0","html_url":"u"}`
	if info := c.Check(context.Background()); info.Available {
		t.Fatalf("same version: %+v", info)
	}
	// No release yet.
	status = http.StatusNotFound
	if info := New(srv.URL, "0.1.0", slog.Default()).Check(context.Background()); info.Available || info.Error != "no releases published yet" {
		t.Fatalf("404: %+v", info)
	}
}
