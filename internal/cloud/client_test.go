package cloud

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPAuthGzipAndErrorRedaction(t *testing.T) {
	secret := "sensitive-fixture"
	mode := "ok"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "default" || password != secret {
			t.Error("missing authentication")
		}
		if mode == "error" {
			w.WriteHeader(500)
			if _, e := io.WriteString(w, secret); e != nil {
				t.Error(e)
			}
			return
		}
		if r.Header.Get("Content-Encoding") == "gzip" {
			z, e := gzip.NewReader(r.Body)
			if e != nil {
				t.Error(e)
				return
			}
			b, e := io.ReadAll(z)
			if e != nil {
				t.Error(e)
			}
			if e = z.Close(); e != nil {
				t.Error(e)
			}
			if string(b) != "payload" || r.URL.Query().Get("query") != "INSERT" {
				t.Error("bad gzip/query")
			}
		}
		if _, e := io.WriteString(w, "1\n"); e != nil {
			t.Error(e)
		}
	}))
	defer server.Close()
	client, e := New(Config{Host: "example.com", Secure: true, Password: secret})
	if e != nil {
		t.Fatal(e)
	}
	client.endpoint = server.URL
	client.http = server.Client()
	if e = client.Ping(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = client.Request(context.Background(), "INSERT", []byte("payload"), true); e != nil {
		t.Fatal(e)
	}
	mode = "error"
	if _, e = client.Request(context.Background(), "SELECT 1", nil, false); e == nil || strings.Contains(e.Error(), secret) {
		t.Fatal("error body leaked", e)
	}
}
func TestRedirectIsNotFollowed(t *testing.T) {
	followed := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	c, e := New(Config{Host: "example.com", Secure: true})
	if e != nil {
		t.Fatal(e)
	}
	c.endpoint = server.URL
	c.http.Transport = server.Client().Transport
	if e = c.Ping(context.Background()); e == nil || followed {
		t.Fatal("redirect followed", e)
	}
}
func TestRejectsUnsafeEndpoints(t *testing.T) {
	for _, c := range []Config{{Host: "example.com", Secure: false}, {Host: "https://example.com", Secure: true}, {Host: "example.com/path", Secure: true}, {Host: "example.com", Port: 70000, Secure: true}} {
		if _, e := New(c); e == nil {
			t.Fatal("unsafe endpoint accepted")
		}
	}
}
