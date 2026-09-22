package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := guard(ok)
	cases := []struct {
		name   string
		method string
		host   string
		hdr    map[string]string
		want   int
	}{
		{"get local", "GET", "127.0.0.1:8787", nil, 200},
		{"get from a link elsewhere", "GET", "127.0.0.1:8787", map[string]string{"Sec-Fetch-Site": "cross-site"}, 200},
		{"get localhost", "GET", "localhost:8787", nil, 200},
		{"rebinding host", "GET", "evil.example:8787", nil, 403},
		{"cross-site form post", "POST", "127.0.0.1:8787", nil, 403},
		{"htmx post", "POST", "127.0.0.1:8787", map[string]string{"HX-Request": "true", "Sec-Fetch-Site": "same-origin"}, 200},
		{"cross-site fetch with header", "POST", "127.0.0.1:8787", map[string]string{"HX-Request": "true", "Sec-Fetch-Site": "cross-site"}, 403},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://"+c.host+"/x", nil)
		req.Host = c.host
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d", c.name, rec.Code, c.want)
		}
	}
}
