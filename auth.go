package main

// Users, identified by Cloudflare Access.
//
// Behind a Cloudflare Tunnel with Access, every request carries a signed JWT
// (Cf-Access-Jwt-Assertion) with the visitor's email. We verify it (RS256,
// keys from the team's JWKS endpoint, issuer and audience checked) and use
// the email as the user id, so each person gets their own history.
//
// Requests that do not come through Cloudflare (direct LAN access, ffmpeg
// reading the stream from loopback) have no JWT and act as the first admin.
// They are recognised by the absence of any Cf-* header; everything that
// really crossed Cloudflare carries Cf-Ray, so it can't pass as LAN.
//
// Without BITPLAY_CF_TEAM_DOMAIN the app stays single-user: everyone is the
// local admin, exactly as before.
//
//	BITPLAY_CF_TEAM_DOMAIN  e.g. myteam.cloudflareaccess.com
//	BITPLAY_CF_AUD          the Access application's AUD tag
//	BITPLAY_ADMINS          comma separated admin emails

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	usersFile       = "config/users.json"
	localUserID     = "local"
	jwksRefetchGap  = time.Minute
	jwtClockSkew    = 60 * time.Second
	maxDisplayName  = 40
	cfAccessJWTHead = "Cf-Access-Jwt-Assertion"
)

type user struct {
	ID    string `json:"email"`
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
}

type userCtxKey struct{}

// currentUser returns the user the identity middleware attached to the
// request; outside it (tests, single-user mode) that is the local admin.
func currentUser(r *http.Request) *user {
	if u, ok := r.Context().Value(userCtxKey{}).(*user); ok {
		return u
	}
	return userFor(localUserID)
}

// ---------------------------------------------------------------------------
// Admins and display names
// ---------------------------------------------------------------------------

func adminEmails() []string {
	var out []string
	for _, e := range strings.Split(os.Getenv("BITPLAY_ADMINS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// firstAdminID is the owner of data created before users existed and the
// identity used for LAN requests.
func firstAdminID() string {
	if a := adminEmails(); len(a) > 0 {
		return a[0]
	}
	return localUserID
}

func isAdmin(id string) bool {
	if id == localUserID {
		return true
	}
	for _, a := range adminEmails() {
		if a == id {
			return true
		}
	}
	return false
}

var userNames = struct {
	sync.Mutex
	loaded bool
	m      map[string]string
}{m: map[string]string{}}

func loadUserNamesLocked() {
	if userNames.loaded {
		return
	}
	userNames.loaded = true
	if b, err := os.ReadFile(usersFile); err == nil {
		if err := json.Unmarshal(b, &userNames.m); err != nil {
			log.Printf("Users: ignoring unreadable %s: %v", usersFile, err)
			userNames.m = map[string]string{}
		}
	}
}

// defaultDisplayName turns "jorge.castro@x.com" into "Jorge Castro".
func defaultDisplayName(id string) string {
	if id == localUserID {
		return "Local"
	}
	local := id
	if i := strings.IndexByte(id, '@'); i > 0 {
		local = id[:i]
	}
	words := strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == '+' })
	for i, w := range words {
		rs := []rune(w)
		rs[0] = unicode.ToUpper(rs[0])
		words[i] = string(rs)
	}
	if len(words) == 0 {
		return id
	}
	return strings.Join(words, " ")
}

func userFor(id string) *user {
	userNames.Lock()
	loadUserNamesLocked()
	name := userNames.m[id]
	userNames.Unlock()
	if name == "" {
		name = defaultDisplayName(id)
	}
	return &user{ID: id, Name: name, Admin: isAdmin(id)}
}

func setUserName(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxDisplayName {
		return fmt.Errorf("name must be 1-%d characters", maxDisplayName)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("name contains invalid characters")
		}
	}
	userNames.Lock()
	defer userNames.Unlock()
	loadUserNamesLocked()
	userNames.m[id] = name
	b, err := json.MarshalIndent(userNames.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(usersFile), 0755); err != nil {
		return err
	}
	tmp := usersFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, usersFile)
}

// ---------------------------------------------------------------------------
// Cloudflare Access JWT verification
// ---------------------------------------------------------------------------

type cfVerifier struct {
	baseURL string // https://<team domain>; also the expected issuer
	aud     string
	client  *http.Client

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func newCFVerifier(teamDomain, aud string) *cfVerifier {
	teamDomain = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(teamDomain), "https://"), "http://"), "/")
	return &cfVerifier{
		baseURL: "https://" + teamDomain,
		aud:     aud,
		// direct, never through the torrent SOCKS proxy
		client: &http.Client{Timeout: 10 * time.Second},
		keys:   map[string]*rsa.PublicKey{},
	}
}

func (v *cfVerifier) refreshKeys() error {
	resp, err := v.client.Get(v.baseURL + "/cdn-cgi/access/certs")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&doc); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("JWKS has no usable RSA keys")
	}
	v.keys, v.fetchedAt = keys, time.Now()
	return nil
}

func (v *cfVerifier) keyFor(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k := v.keys[kid]; k != nil {
		return k, nil
	}
	// unknown kid: Cloudflare may have rotated keys, but don't let a bad
	// token make us hammer the endpoint
	if time.Since(v.fetchedAt) < jwksRefetchGap {
		return nil, errors.New("unknown signing key")
	}
	v.fetchedAt = time.Now()
	if err := v.refreshKeys(); err != nil {
		return nil, err
	}
	if k := v.keys[kid]; k != nil {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

// Verify checks the token and returns the lowercased email claim.
func (v *cfVerifier) Verify(token string, now time.Time) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeJWTPart(parts[0], &header); err != nil {
		return "", err
	}
	if header.Alg != "RS256" {
		return "", errors.New("unsupported algorithm")
	}
	key, err := v.keyFor(header.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("malformed signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return "", errors.New("bad signature")
	}

	var claims struct {
		Iss   string          `json:"iss"`
		Aud   json.RawMessage `json:"aud"`
		Exp   float64         `json:"exp"`
		Nbf   float64         `json:"nbf"`
		Email string          `json:"email"`
	}
	if err := decodeJWTPart(parts[1], &claims); err != nil {
		return "", err
	}
	if claims.Iss != v.baseURL {
		return "", errors.New("wrong issuer")
	}
	if !audContains(claims.Aud, v.aud) {
		return "", errors.New("wrong audience")
	}
	if claims.Exp == 0 || now.After(time.Unix(int64(claims.Exp), 0).Add(jwtClockSkew)) {
		return "", errors.New("token expired")
	}
	if claims.Nbf != 0 && now.Add(jwtClockSkew).Before(time.Unix(int64(claims.Nbf), 0)) {
		return "", errors.New("token not valid yet")
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" || !strings.Contains(email, "@") {
		return "", errors.New("token has no email")
	}
	return email, nil
}

func decodeJWTPart(part string, out interface{}) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return errors.New("malformed token")
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errors.New("malformed token")
	}
	return nil
}

func audContains(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// viaCloudflare: anything that crossed Cloudflare's edge carries these.
func viaCloudflare(r *http.Request) bool {
	for _, h := range []string{"Cf-Ray", "Cf-Connecting-Ip", "Cf-Visitor", "Cf-Access-Jwt-Assertion", "Cf-Access-Authenticated-User-Email"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

func fromPrivateAddr(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func serveAs(next http.Handler, w http.ResponseWriter, r *http.Request, u *user) {
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey{}, u)))
}

// identityMiddleware attaches the user to every request (see the file
// comment). Without a Cloudflare team domain it is a single-user no-op.
func identityMiddleware(next http.Handler) http.Handler {
	team := strings.TrimSpace(os.Getenv("BITPLAY_CF_TEAM_DOMAIN"))
	if team == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveAs(next, w, r, userFor(firstAdminID()))
		})
	}
	aud := strings.TrimSpace(os.Getenv("BITPLAY_CF_AUD"))
	if aud == "" {
		log.Fatal("BITPLAY_CF_TEAM_DOMAIN is set but BITPLAY_CF_AUD is empty")
	}
	log.Printf("Cloudflare Access enabled for %s (admins: %v)", team, adminEmails())
	return identityHandler(next, newCFVerifier(team, aud))
}

func identityHandler(next http.Handler, v *cfVerifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := r.Header.Get(cfAccessJWTHead); token != "" {
			email, err := v.Verify(token, time.Now())
			if err != nil {
				log.Printf("Auth: rejected Cloudflare token: %v", err)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			serveAs(next, w, r, userFor(email))
			return
		}
		// no token: only direct local access (LAN, ffmpeg on loopback)
		if viaCloudflare(r) || !fromPrivateAddr(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		serveAs(next, w, r, userFor(firstAdminID()))
	})
}

// requireAdmin restricts a handler to admins.
func requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).Admin {
			respondWithJSON(w, http.StatusForbidden, map[string]string{"error": "Admin only"})
			return
		}
		h(w, r)
	}
}

// meHandler: GET /api/v1/me, POST /api/v1/me {name}
func meHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var in struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
			return
		}
		if err := setUserName(u.ID, in.Name); err != nil {
			respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		u = userFor(u.ID)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondWithJSON(w, http.StatusOK, u)
}
