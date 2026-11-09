package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCastTokenRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	claims := castClaims{Session: "abc", File: 3, Exp: now.Add(time.Hour).Unix(),
		Subs: []castSub{{Kind: "os", ID: "123", Lang: "es", Name: "Español"}}}
	token, err := signCastToken(key, claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifyCastToken(key, token, now)
	if err != nil || got.Session != "abc" || got.File != 3 || len(got.Subs) != 1 || got.Subs[0].ID != "123" {
		t.Fatalf("round trip = %+v, %v", got, err)
	}

	payload, sig, _ := strings.Cut(token, ".")
	cases := map[string]string{
		"expired":        token,
		"other key":      token,
		"tampered":       payload[:len(payload)-2] + "AA." + sig,
		"no signature":   payload + ".",
		"no dot":         payload,
		"empty":          "",
		"garbage":        "a.b",
		"swapped parts":  sig + "." + payload,
		"signature only": "." + sig,
	}
	for name, tok := range cases {
		k, at := key, now
		switch name {
		case "expired":
			at = now.Add(2 * time.Hour)
		case "other key":
			k = []byte("ffffffffffffffffffffffffffffffff")
		}
		if _, err := verifyCastToken(k, tok, at); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCastLangTagAndNames(t *testing.T) {
	for in, want := range map[string]string{"es": "es", "EA": "es-419", "sp": "es-ES", "pt-BR": "pt-BR", "": "und", "x y": "und", "toolongcode": "und"} {
		if got := castLangTag(in); got != want {
			t.Errorf("castLangTag(%q) = %q, want %q", in, got, want)
		}
	}
	if got := castCleanName("  Español \"LA\"\n (OpenSubtitles) ", "x"); got != "Español LA (OpenSubtitles)" {
		t.Errorf("name = %q", got)
	}
	if got := castCleanName("\"\"", "ES"); got != "ES" {
		t.Errorf("fallback = %q", got)
	}
	if got := castCleanName(strings.Repeat("ñ", 80), "x"); len([]rune(got)) != 40 {
		t.Errorf("long name has %d runes", len([]rune(got)))
	}
}

func TestCleanCastSubs(t *testing.T) {
	files := map[int]string{0: "Movie.mkv", 1: "Subs/es.srt", 2: "Subs/en.VTT", 3: "readme.txt"}
	lookup := func(i int) (string, bool) { n, ok := files[i]; return n, ok }
	in := []castSub{
		{Kind: "os", ID: "123", Lang: "es", Name: "Español"},
		{Kind: "os", ID: "12x", Lang: "es"},           // not numeric
		{Kind: "os", ID: "1234567890123", Lang: "es"}, // too long
		{Kind: "tf", File: 1, Lang: "es", Name: "Torrent"},
		{Kind: "tf", File: 2, Lang: "en"},
		{Kind: "tf", File: 3, Lang: "en"},   // not a subtitle
		{Kind: "tf", File: 9, Lang: "en"},   // no such file
		{Kind: "evil", ID: "1", Lang: "en"}, // unknown kind
	}
	got := cleanCastSubs(in, lookup)
	if len(got) != 3 || got[0].ID != "123" || got[1].File != 1 || got[2].File != 2 {
		t.Fatalf("subs = %+v", got)
	}
	if got[2].Name != "EN" {
		t.Errorf("a missing name falls back to the language: %q", got[2].Name)
	}

	var many []castSub
	for i := 0; i < 20; i++ {
		many = append(many, castSub{Kind: "os", ID: "1", Lang: "es"})
	}
	if n := len(cleanCastSubs(many, lookup)); n != castMaxSubs {
		t.Errorf("kept %d subs, want %d", n, castMaxSubs)
	}
}

func TestCastMasterPlaylist(t *testing.T) {
	subs := []castSub{{Kind: "os", ID: "1", Lang: "es", Name: "Español"}, {Kind: "os", ID: "2", Lang: "ea", Name: "Latino"}}
	pl := castMasterPlaylist(subs, 3)
	for _, want := range []string{
		"#EXTM3U",
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Español",LANGUAGE="es",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI="subs/0.m3u8"`,
		`NAME="Latino",LANGUAGE="es-419",DEFAULT=NO,AUTOSELECT=YES,FORCED=NO,URI="subs/1.m3u8"`,
		`CODECS="avc1.640029,mp4a.40.2",SUBTITLES="subs"` + "\nindex.m3u8?audio=3\n",
	} {
		if !strings.Contains(pl, want) {
			t.Errorf("master playlist is missing %q:\n%s", want, pl)
		}
	}

	plain := castMasterPlaylist(nil, -1)
	if strings.Contains(plain, "SUBTITLES") || strings.Contains(plain, "EXT-X-MEDIA") || !strings.HasSuffix(plain, "index.m3u8\n") {
		t.Errorf("playlist without subtitles:\n%s", plain)
	}
}

func TestCastSubPlaylistAndTimestampMap(t *testing.T) {
	pl := castSubPlaylist(5400.4, 2)
	for _, want := range []string{"#EXT-X-TARGETDURATION:5401", "#EXTINF:5400.400,\n2.seg.vtt\n", "#EXT-X-ENDLIST"} {
		if !strings.Contains(pl, want) {
			t.Errorf("sub playlist is missing %q:\n%s", want, pl)
		}
	}
	if !strings.Contains(castSubPlaylist(0, 0), "TARGETDURATION:14400") {
		t.Error("unknown duration must fall back to a long one")
	}

	const header = "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n"
	for name, in := range map[string]string{
		"plain":        "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHola\n",
		"bom and crlf": "\uFEFFWEBVTT\r\n\r\n00:00:01.000 --> 00:00:02.000\r\nHola\r\n",
	} {
		got := string(withTimestampMap([]byte(in)))
		if !strings.HasPrefix(got, header+"\n00:00:01.000 --> 00:00:02.000\nHola") {
			t.Errorf("%s: %q", name, got)
		}
	}
	already := "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000\n\n00:00:01.000 --> 00:00:02.000\nHola\n"
	if got := string(withTimestampMap([]byte(already))); got != already {
		t.Errorf("an existing map must stay: %q", got)
	}
	if got := string(withTimestampMap([]byte("00:00:01.000 --> 00:00:02.000\nNo header\n"))); !strings.HasPrefix(got, header+"\n00:00:01.000") {
		t.Errorf("missing header: %q", got)
	}
}

// castSession registers a fake session and returns a token for it.
func castSession(t *testing.T, subs []castSub) (token string) {
	t.Helper()
	t.Chdir(t.TempDir()) // castKey writes config/cast.key here
	castSecret.key = nil
	castSecret.once = sync.Once{}
	sessions.Store("sess1", &TorrentSession{ProbeCache: map[int]*ProbeResult{0: {Duration: 5400}}})
	t.Cleanup(func() { sessions.Delete("sess1") })
	token, err := signCastToken(castKey(), castClaims{Session: "sess1", File: 0, Exp: time.Now().Add(time.Hour).Unix(), Subs: subs})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func castGet(token, path, method string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, castPathPrefix+token+"/"+path, nil)
	rec := httptest.NewRecorder()
	castHandler(rec, req)
	return rec
}

func TestCastHandlerPlaylistsAndSubtitles(t *testing.T) {
	subs := []castSub{{Kind: "os", ID: "424242", Lang: "es", Name: "Español"}}
	token := castSession(t, subs)
	subtitleCachePut("424242", []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHola\n"))

	rec := castGet(token, "hls/master.m3u8?audio=2", http.MethodGet)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" ||
		!strings.Contains(rec.Body.String(), "index.m3u8?audio=2") || !strings.Contains(rec.Body.String(), `URI="subs/0.m3u8"`) {
		t.Errorf("master: %d %s\n%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("receivers need CORS")
	}

	rec = castGet(token, "hls/subs/0.m3u8", http.MethodGet)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "#EXTINF:5400.000,\n0.seg.vtt") {
		t.Errorf("sub playlist: %d\n%s", rec.Code, rec.Body.String())
	}
	rec = castGet(token, "hls/subs/0.seg.vtt", http.MethodGet)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/vtt") {
		t.Errorf("segment vtt: %d\n%s", rec.Code, rec.Body.String())
	}
	rec = castGet(token, "hls/subs/0.vtt", http.MethodGet)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "X-TIMESTAMP-MAP") || !strings.Contains(rec.Body.String(), "Hola") {
		t.Errorf("plain vtt: %d\n%s", rec.Code, rec.Body.String())
	}

	for _, path := range []string{"hls/subs/1.vtt", "hls/subs/-1.m3u8", "hls/subs/x.vtt", "hls/subs/0.txt", "nope", "hls/segx.mp4", "hls/"} {
		if rec := castGet(token, path, http.MethodGet); rec.Code != 404 {
			t.Errorf("%s: code %d, want 404", path, rec.Code)
		}
	}
}

func TestCastHandlerAccessRules(t *testing.T) {
	token := castSession(t, nil)

	if rec := castGet(token, "hls/master.m3u8", http.MethodOptions); rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Errorf("preflight: %d", rec.Code)
	}
	if rec := castGet(token, "hls/master.m3u8", http.MethodPost); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
	if rec := castGet(token+"x", "hls/master.m3u8", http.MethodGet); rec.Code != http.StatusForbidden {
		t.Errorf("tampered token: %d", rec.Code)
	}
	if rec := castGet("garbage", "hls/master.m3u8", http.MethodGet); rec.Code != http.StatusForbidden {
		t.Errorf("garbage token: %d", rec.Code)
	}

	expired, _ := signCastToken(castKey(), castClaims{Session: "sess1", Exp: time.Now().Add(-time.Minute).Unix()})
	if rec := castGet(expired, "hls/master.m3u8", http.MethodGet); rec.Code != http.StatusForbidden {
		t.Errorf("expired token: %d", rec.Code)
	}

	other, _ := signCastToken(castKey(), castClaims{Session: "closed", Exp: time.Now().Add(time.Hour).Unix()})
	if rec := castGet(other, "hls/master.m3u8", http.MethodGet); rec.Code != http.StatusNotFound {
		t.Errorf("closed session: %d", rec.Code)
	}

	// a token for one file can't ask for subtitles it doesn't list
	if rec := castGet(token, "hls/subs/0.vtt", http.MethodGet); rec.Code != 404 {
		t.Errorf("subtitle outside the token: %d", rec.Code)
	}
}

func TestCastRouterSkipsLoginAndSigningKeyPersists(t *testing.T) {
	token := castSession(t, nil)
	loggedIn := false
	h := castRouter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loggedIn = true // the normal chain
	}))

	// a /cast/ request with Cloudflare headers and no JWT: the identity
	// middleware would reject it, castRouter must not even reach it
	req := httptest.NewRequest(http.MethodGet, castPathPrefix+token+"/hls/master.m3u8", nil)
	req.Header.Set("Cf-Ray", "abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if loggedIn || rec.Code != 200 {
		t.Errorf("cast link: login chain reached=%v code=%d", loggedIn, rec.Code)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if !loggedIn {
		t.Error("other paths must still go through the login chain")
	}

	// a restart (new process) reads the saved key, so the same link works
	castSecret.key = nil
	castSecret.once = sync.Once{}
	if rec := castGet(token, "hls/master.m3u8", http.MethodGet); rec.Code != 200 {
		t.Errorf("after restart: %d", rec.Code)
	}
}

func TestCastPublicBase(t *testing.T) {
	t.Setenv("BITPLAY_PUBLIC_URL", "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cast/token", nil)
	req.Host = "yts.jrg9.com"
	if got := castPublicBase(req); got != "http://yts.jrg9.com" {
		t.Errorf("plain = %s", got)
	}
	req.Header.Set("Cf-Visitor", `{"scheme":"https"}`)
	if got := castPublicBase(req); got != "https://yts.jrg9.com" {
		t.Errorf("behind Cloudflare = %s", got)
	}
	t.Setenv("BITPLAY_PUBLIC_URL", "https://cast.example.com/")
	if got := castPublicBase(req); got != "https://cast.example.com" {
		t.Errorf("configured = %s", got)
	}
	t.Setenv("BITPLAY_PUBLIC_URL", "javascript:alert(1)")
	if got := castPublicBase(req); got != "https://yts.jrg9.com" {
		t.Errorf("invalid configured value = %s", got)
	}
}

func TestCastTokenHandlerValidation(t *testing.T) {
	do := func(method, body string) int {
		rec := httptest.NewRecorder()
		castTokenHandler(rec, httptest.NewRequest(method, "/api/v1/cast/token", strings.NewReader(body)))
		return rec.Code
	}
	if code := do(http.MethodGet, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", code)
	}
	if code := do(http.MethodPost, "nope"); code != http.StatusBadRequest {
		t.Errorf("bad body: %d", code)
	}
	if code := do(http.MethodPost, `{"session":"missing","file":0}`); code != http.StatusNotFound {
		t.Errorf("unknown session: %d", code)
	}
}
