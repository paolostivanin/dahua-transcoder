package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
)

// digestClient performs HTTP Digest authentication (RFC 7616). Dahua NVRs
// offer SHA-256, which Python before 3.14 and many small clients cannot
// answer, so SHA-256 is preferred whenever a challenge offers it; MD5 is the
// fallback. Only GET without a body is needed here.
type digestClient struct {
	client   *http.Client
	username string
	password string
}

type digestChallenge struct {
	realm     string
	nonce     string
	opaque    string
	algorithm string // canonical upper case: MD5, MD5-SESS, SHA-256, SHA-256-SESS
	qopAuth   bool
}

func (d *digestClient) Get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenges := resp.Header.Values("WWW-Authenticate")
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()

	ch, err := pickChallenge(challenges)
	if err != nil {
		return nil, err
	}
	cnonce, err := newCnonce()
	if err != nil {
		return nil, err
	}
	authz, err := ch.authorization(http.MethodGet, req.URL.RequestURI(), d.username, d.password, cnonce, 1)
	if err != nil {
		return nil, err
	}
	req2 := req.Clone(ctx)
	req2.Header.Set("Authorization", authz)
	return d.client.Do(req2)
}

func newCnonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// pickChallenge chooses the strongest supported Digest challenge.
func pickChallenge(headers []string) (*digestChallenge, error) {
	var best *digestChallenge
	rank := func(alg string) int {
		switch alg {
		case "SHA-256", "SHA-256-SESS":
			return 2
		case "MD5", "MD5-SESS":
			return 1
		}
		return 0
	}
	for _, h := range headers {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
		if !strings.EqualFold(scheme, "Digest") {
			continue
		}
		params := parseAuthParams(rest)
		ch := &digestChallenge{
			realm:     params["realm"],
			nonce:     params["nonce"],
			opaque:    params["opaque"],
			algorithm: strings.ToUpper(params["algorithm"]),
		}
		if ch.algorithm == "" {
			ch.algorithm = "MD5"
		}
		if qop, ok := params["qop"]; ok {
			for _, q := range strings.Split(qop, ",") {
				if strings.EqualFold(strings.TrimSpace(q), "auth") {
					ch.qopAuth = true
				}
			}
			if !ch.qopAuth {
				continue // only auth-int offered; not supported
			}
		}
		if ch.nonce == "" || rank(ch.algorithm) == 0 {
			continue
		}
		if best == nil || rank(ch.algorithm) > rank(best.algorithm) {
			best = ch
		}
	}
	if best == nil {
		return nil, errors.New("recorder offered no supported digest challenge")
	}
	return best, nil
}

// parseAuthParams parses `key=value, key="quoted, value"` lists.
func parseAuthParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,\t")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimLeft(s[eq+1:], " \t")
		var val string
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(s); i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					b.WriteByte(s[i])
					continue
				}
				if s[i] == '"' {
					break
				}
				b.WriteByte(s[i])
			}
			val = b.String()
			if i < len(s) {
				i++
			}
			s = s[i:]
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val = strings.TrimSpace(s[:end])
			s = s[end:]
		}
		out[key] = val
	}
	return out
}

func (c *digestChallenge) authorization(method, uri, user, pass, cnonce string, nc int) (string, error) {
	var newHash func() hash.Hash
	switch c.algorithm {
	case "SHA-256", "SHA-256-SESS":
		newHash = sha256.New
	case "MD5", "MD5-SESS":
		newHash = md5.New
	default:
		return "", fmt.Errorf("unsupported digest algorithm %q", c.algorithm)
	}
	h := func(s string) string {
		x := newHash()
		x.Write([]byte(s))
		return hex.EncodeToString(x.Sum(nil))
	}
	ha1 := h(user + ":" + c.realm + ":" + pass)
	if strings.HasSuffix(c.algorithm, "-SESS") {
		ha1 = h(ha1 + ":" + c.nonce + ":" + cnonce)
	}
	ha2 := h(method + ":" + uri)
	ncs := fmt.Sprintf("%08x", nc)
	var response string
	if c.qopAuth {
		response = h(ha1 + ":" + c.nonce + ":" + ncs + ":" + cnonce + ":auth:" + ha2)
	} else {
		response = h(ha1 + ":" + c.nonce + ":" + ha2)
	}
	q := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
	parts := []string{
		"username=" + q(user),
		"realm=" + q(c.realm),
		"nonce=" + q(c.nonce),
		"uri=" + q(uri),
		"algorithm=" + c.algorithm,
		"response=" + q(response),
	}
	if c.qopAuth {
		parts = append(parts, "qop=auth", "nc="+ncs, "cnonce="+q(cnonce))
	}
	if c.opaque != "" {
		parts = append(parts, "opaque="+q(c.opaque))
	}
	return "Digest " + strings.Join(parts, ", "), nil
}
