package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type jwtFixture struct {
	key      *rsa.PrivateKey
	srv      *httptest.Server
	fetches  atomic.Int32
	verifier *cfVerifier
}

func newJWTFixture(t *testing.T) *jwtFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &jwtFixture{key: key}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.fetches.Add(1)
		json.NewEncoder(w).Encode(map[string]interface{}{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(f.srv.Close)
	f.verifier = &cfVerifier{baseURL: f.srv.URL, aud: "AUD123", client: f.srv.Client(), keys: map[string]*rsa.PublicKey{}}
	return f
}

func b64json(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// token signs claims with key (kid/alg configurable for the negative cases).
func (f *jwtFixture) token(t *testing.T, key *rsa.PrivateKey, alg, kid string, claims map[string]interface{}) string {
	t.Helper()
	signing := b64json(t, map[string]string{"alg": alg, "kid": kid, "typ": "JWT"}) + "." + b64json(t, claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *jwtFixture) claims(mod func(map[string]interface{})) map[string]interface{} {
	c := map[string]interface{}{
		"iss":   f.srv.URL,
		"aud":   []string{"AUD123"},
		"email": "Jorge@Example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"nbf":   time.Now().Add(-time.Minute).Unix(),
	}
	if mod != nil {
		mod(c)
	}
	return c
}

func TestVerifyJWT(t *testing.T) {
	f := newJWTFixture(t)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token string
		want  string // email, or "" when it must be rejected
	}{
		{"valid", f.token(t, f.key, "RS256", "k1", f.claims(nil)), "jorge@example.com"},
		{"aud as a string", f.token(t, f.key, "RS256", "k1", f.claims(func(c map[string]interface{}) { c["aud"] = "AUD123" })), "jorge@example.com"},
		{"wrong audience", f.token(t, f.key, "RS256", "k1", f.claims(func(c map[string]interface{}) { c["aud"] = []string{"other"} })), ""},
		{"wrong issuer", f.token(t, f.key, "RS256", "k1", f.claims(func(c map[string]interface{}) { c["iss"] = "https://evil.example" })), ""},
		{"expired", f.token(t, f.key, "RS256", "k1", f.claims(func(c map[string]interface{}) { c["exp"] = time.Now().Add(-time.Hour).Unix() })), ""},
		{"not yet valid", f.token(t, f.key, "RS256", "k1", f.claims(func(c map[string]interface{}) { c["nbf"] = time.Now().Add(time.Hour).Unix() })), ""},
		{"no email", f.token(t, f.key, "RS256", "k1", f.claims(func(c map[string]interface{}) { delete(c, "email") })), ""},
		{"signed by another key", f.token(t, otherKey, "RS256", "k1", f.claims(nil)), ""},
		{"alg none", f.token(t, f.key, "none", "k1", f.claims(nil)), ""},
		{"alg HS256", f.token(t, f.key, "HS256", "k1", f.claims(nil)), ""},
		{"unknown kid", f.token(t, f.key, "RS256", "zzz", f.claims(nil)), ""},
		{"garbage", "not.a.jwt", ""},
		{"two parts", "a.b", ""},
	}
	for _, c := range cases {
		got, err := f.verifier.Verify(c.token, time.Now())
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: accepted as %q", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

// Unknown key ids must not make us hit the JWKS endpoint on every request.
func TestJWKSRefetchIsRateLimited(t *testing.T) {
	f := newJWTFixture(t)
	bad := f.token(t, f.key, "RS256", "zzz", f.claims(nil))
	for i := 0; i < 5; i++ {
		f.verifier.Verify(bad, time.Now())
	}
	if n := f.fetches.Load(); n != 1 {
		t.Fatalf("JWKS fetched %d times for repeated unknown kids, want 1", n)
	}
}

func TestIdentityMiddleware(t *testing.T) {
	t.Setenv("BITPLAY_ADMINS", "boss@example.com")
	f := newJWTFixture(t)

	var seen *user
	h := identityHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = currentUser(r)
		w.WriteHeader(http.StatusOK)
	}), f.verifier)

	do := func(remote string, headers map[string]string) int {
		seen = nil
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.RemoteAddr = remote
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	jwtFor := func(email string) string {
		return f.token(t, f.key, "RS256", "k1", f.claims(func(c map[string]interface{}) { c["email"] = email }))
	}

	if code := do("172.18.0.5:5555", map[string]string{"Cf-Access-Jwt-Assertion": jwtFor("Visitor@x.com"), "Cf-Ray": "abc"}); code != 200 ||
		seen.ID != "visitor@x.com" || seen.Admin || seen.Name != "Visitor" {
		t.Errorf("visitor via Cloudflare: code %d user %+v", code, seen)
	}
	if code := do("172.18.0.5:5555", map[string]string{"Cf-Access-Jwt-Assertion": jwtFor("boss@example.com")}); code != 200 || !seen.Admin {
		t.Errorf("admin via Cloudflare: code %d user %+v", code, seen)
	}
	if code := do("172.18.0.5:5555", map[string]string{"Cf-Access-Jwt-Assertion": "x.y.z"}); code != 401 {
		t.Errorf("invalid JWT: code %d", code)
	}
	// crossed Cloudflare without Access: must not pass as LAN
	if code := do("172.18.0.5:5555", map[string]string{"Cf-Ray": "abc"}); code != 401 {
		t.Errorf("Cf-Ray without JWT: code %d", code)
	}
	if code := do("172.18.0.5:5555", map[string]string{"Cf-Access-Authenticated-User-Email": "boss@example.com"}); code != 401 {
		t.Errorf("forged email header: code %d", code)
	}
	// direct LAN / loopback access acts as the first admin
	for _, remote := range []string{"192.168.1.20:4000", "127.0.0.1:4000", "[::1]:4000", "10.0.0.7:1"} {
		if code := do(remote, nil); code != 200 || seen.ID != "boss@example.com" || !seen.Admin {
			t.Errorf("LAN %s: code %d user %+v", remote, code, seen)
		}
	}
	// a public address without a token is never trusted
	if code := do("8.8.8.8:4000", nil); code != 401 {
		t.Errorf("public address without JWT: code %d", code)
	}
}

func TestRequireAdmin(t *testing.T) {
	t.Setenv("BITPLAY_ADMINS", "boss@example.com")
	ok := requireAdmin(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	f := newJWTFixture(t)
	h := identityHandler(ok, f.verifier)

	for email, want := range map[string]int{"boss@example.com": 204, "visitor@x.com": 403} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/proxy", nil)
		req.RemoteAddr = "172.18.0.5:1"
		req.Header.Set("Cf-Access-Jwt-Assertion", f.token(t, f.key, "RS256", "k1", f.claims(func(c map[string]interface{}) { c["email"] = email })))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: code %d, want %d", email, rec.Code, want)
		}
	}
}

func TestDisplayNames(t *testing.T) {
	for in, want := range map[string]string{
		"jorge.castro@x.com": "Jorge Castro",
		"maria_lopez@x.com":  "Maria Lopez",
		"bob@x.com":          "Bob",
		localUserID:          "Local",
	} {
		if got := defaultDisplayName(in); got != want {
			t.Errorf("defaultDisplayName(%q) = %q, want %q", in, got, want)
		}
	}

	t.Chdir(t.TempDir())
	userNames.Lock()
	userNames.loaded, userNames.m = false, map[string]string{}
	userNames.Unlock()
	if err := setUserName("a@x.com", "  Ana María  "); err != nil {
		t.Fatal(err)
	}
	if got := userFor("a@x.com").Name; got != "Ana María" {
		t.Errorf("saved name = %q", got)
	}
	for _, bad := range []string{"", "   ", "a\x00b", string(make([]rune, 41))} {
		if err := setUserName("a@x.com", bad); err == nil {
			t.Errorf("setUserName(%q) should fail", bad)
		}
	}
}
