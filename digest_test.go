package main

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RFC 7616 section 3.9.1 example: same request answered with MD5 and SHA-256.
func TestDigestRFC7616Vectors(t *testing.T) {
	for _, tc := range []struct{ alg, want string }{
		{"MD5", "8ca523f5e9506fed4657c9700eebdbec"},
		{"SHA-256", "753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"},
	} {
		ch := &digestChallenge{
			realm:     "http-auth@example.org",
			nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
			opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
			algorithm: tc.alg,
			qopAuth:   true,
		}
		got, err := ch.authorization("GET", "/dir/index.html", "Mufasa", "Circle of Life",
			"f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ", 1)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `response="`+tc.want+`"`) {
			t.Errorf("%s: got %s, want response %s", tc.alg, got, tc.want)
		}
		for _, part := range []string{`qop=auth`, `nc=00000001`, `algorithm=` + tc.alg, `opaque="FQhe/`} {
			if !strings.Contains(got, part) {
				t.Errorf("%s: header lacks %s: %s", tc.alg, part, got)
			}
		}
	}
}

func TestParseAuthParams(t *testing.T) {
	p := parseAuthParams(`realm="a, b", qop="auth,auth-int", nonce=abc, opaque="q\"x", algorithm=SHA-256`)
	want := map[string]string{"realm": "a, b", "qop": "auth,auth-int", "nonce": "abc", "opaque": `q"x`, "algorithm": "SHA-256"}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %q, want %q", k, p[k], v)
		}
	}
}

func TestPickChallenge(t *testing.T) {
	ch, err := pickChallenge([]string{
		`Digest realm="r", nonce="n1", qop="auth", algorithm=MD5`,
		`Digest realm="r", nonce="n2", qop="auth", algorithm=SHA-256`,
		`Basic realm="r"`,
	})
	if err != nil || ch.algorithm != "SHA-256" || ch.nonce != "n2" {
		t.Fatalf("got %+v, %v; want the SHA-256 challenge", ch, err)
	}
	ch, err = pickChallenge([]string{`Digest realm="r", nonce="n"`})
	if err != nil || ch.algorithm != "MD5" || ch.qopAuth {
		t.Fatalf("legacy challenge: got %+v, %v", ch, err)
	}
	if _, err := pickChallenge([]string{`Digest realm="r", nonce="n", qop="auth-int"`, `Basic realm="r"`}); err == nil {
		t.Fatal("auth-int only and Basic must be rejected")
	}
}

// digestServer is a minimal RFC 7616 server, checked independently of the
// client code: it recomputes the expected response itself.
type digestServer struct {
	user, pass string
	algs       []string // advertised in this order
	body       func(w http.ResponseWriter, r *http.Request)
	lastQuery  string
	authed     int
}

func (s *digestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const realm, nonce = "Login to NVR", "0123456789abcdef"
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Digest ") && s.valid(h[7:], realm, nonce, r) {
		s.authed++
		s.lastQuery = r.URL.RawQuery
		s.body(w, r)
		return
	}
	for _, alg := range s.algs {
		w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", qop="auth", nonce="%s", opaque="op", algorithm=%s`, realm, nonce, alg))
	}
	w.WriteHeader(http.StatusUnauthorized)
}

func (s *digestServer) valid(h, realm, nonce string, r *http.Request) bool {
	p := parseAuthParams(h)
	var nh func() hash.Hash
	switch p["algorithm"] {
	case "SHA-256":
		nh = sha256.New
	case "MD5":
		nh = md5.New
	default:
		return false
	}
	hx := func(v string) string { x := nh(); io.WriteString(x, v); return hex.EncodeToString(x.Sum(nil)) }
	if p["username"] != s.user || p["realm"] != realm || p["nonce"] != nonce || p["uri"] != r.URL.RequestURI() || p["opaque"] != "op" {
		return false
	}
	ha1 := hx(s.user + ":" + realm + ":" + s.pass)
	ha2 := hx(r.Method + ":" + p["uri"])
	return p["response"] == hx(ha1+":"+nonce+":"+p["nc"]+":"+p["cnonce"]+":auth:"+ha2)
}

func TestDigestClientAgainstServer(t *testing.T) {
	for _, algs := range [][]string{{"MD5", "SHA-256"}, {"MD5"}} {
		s := &digestServer{user: "viewer", pass: `p"ss`, algs: algs, body: func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }}
		srv := httptest.NewServer(s)
		c := &digestClient{client: srv.Client(), username: "viewer", password: `p"ss`}
		resp, err := c.Get(context.Background(), srv.URL+"/cgi-bin/loadfile.cgi?action=startLoad&startTime=2026-10-27%2011:15:23")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(b) != "ok" {
			t.Errorf("%v: status %d body %q", algs, resp.StatusCode, b)
		}

		bad := &digestClient{client: srv.Client(), username: "viewer", password: "wrong"}
		resp, err = bad.Get(context.Background(), srv.URL+"/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%v: wrong password got %d, want 401", algs, resp.StatusCode)
		}
		srv.Close()
	}
}
