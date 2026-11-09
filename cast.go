package main

// Casting to a Chromecast or an Apple TV (AirPlay).
//
// A cast receiver downloads the video itself, with no cookies, so it can't
// pass Cloudflare Access. POST /api/v1/cast/token (normal login) mints a
// signed link that expires after castTokenTTL and only opens one file of
// one torrent session plus the subtitles listed in it:
//
//	/cast/<token>/hls/master.m3u8        HLS master: video + subtitle renditions (AirPlay)
//	/cast/<token>/hls/index.m3u8         the video playlist alone (Chromecast)
//	/cast/<token>/hls/seg<N>.ts          segments (hls.go)
//	/cast/<token>/hls/subs/<N>.m3u8      subtitle playlist (one WebVTT segment)
//	/cast/<token>/hls/subs/<N>.seg.vtt   WebVTT with X-TIMESTAMP-MAP, for that playlist
//	/cast/<token>/hls/subs/<N>.vtt       plain WebVTT (Chromecast side-loads it)
//
// Cloudflare Access must let /cast/* through (a Bypass policy on that path);
// the token is what authorises. These URLs skip the login middlewares.
// The video is the same H.264 + AAC HLS Safari uses, so every receiver
// can play it.
//
//	BITPLAY_PUBLIC_URL  e.g. https://yts.jrg9.com, the address receivers use
//	                    (default: derived from the request)

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	castTokenTTL        = 6 * time.Hour
	castMaxSubs         = 8
	castKeyFile         = "config/cast.key"
	castPathPrefix      = "/cast/"
	castBandwidth       = 5000000 // advertised in the master playlist
	castMaxSubSize      = 10 << 20
	castFallbackRuntime = 4 * 3600.0 // subtitle playlist length when the duration is unknown
)

type castSub struct {
	Kind string `json:"k"`            // "os": OpenSubtitles file id; "tf": subtitle file inside the torrent
	ID   string `json:"id,omitempty"` // os
	File int    `json:"f,omitempty"`  // tf
	Lang string `json:"l"`            // BCP 47 tag
	Name string `json:"n"`
}

type castClaims struct {
	Session string    `json:"s"`
	File    int       `json:"f"`
	Exp     int64     `json:"e"`
	Subs    []castSub `json:"u,omitempty"`
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

var castSecret struct {
	once sync.Once
	key  []byte
}

// castKey is the HMAC key, kept in config/cast.key so links survive a
// restart. If it can't be saved, links last until the next restart.
func castKey() []byte {
	castSecret.once.Do(func() {
		if b, err := os.ReadFile(castKeyFile); err == nil {
			if k, err := hex.DecodeString(strings.TrimSpace(string(b))); err == nil && len(k) >= 32 {
				castSecret.key = k
				return
			}
		}
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			log.Fatalf("Cast: no randomness for the signing key: %v", err)
		}
		castSecret.key = k
		if err := os.MkdirAll(filepath.Dir(castKeyFile), 0755); err == nil {
			err = os.WriteFile(castKeyFile, []byte(hex.EncodeToString(k)+"\n"), 0600)
			if err != nil {
				log.Printf("Cast: could not save %s (links won't survive a restart): %v", castKeyFile, err)
			}
		}
	})
	return castSecret.key
}

func signCastToken(key []byte, c castClaims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func verifyCastToken(key []byte, token string, now time.Time) (*castClaims, error) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok || p == "" || sig == "" {
		return nil, errors.New("malformed token")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(p))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return nil, errors.New("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, errors.New("malformed token")
	}
	var c castClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errors.New("malformed token")
	}
	if now.Unix() >= c.Exp {
		return nil, errors.New("token expired")
	}
	return &c, nil
}

// ---------------------------------------------------------------------------
// Playlists and WebVTT
// ---------------------------------------------------------------------------

var langTagRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)

// castLangTag turns OpenSubtitles codes into BCP 47 ("ea" is Latin American
// Spanish, "sp" is Spain's); unusable input becomes "und".
func castLangTag(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	switch code {
	case "ea":
		return "es-419"
	case "sp":
		return "es-ES"
	}
	if !langTagRe.MatchString(code) {
		return "und"
	}
	// conventional case: "pt-br" -> "pt-BR"
	if lang, region, ok := strings.Cut(code, "-"); ok && len(region) == 2 {
		return lang + "-" + strings.ToUpper(region)
	}
	return code
}

// castCleanName keeps a track name safe inside a quoted playlist attribute.
func castCleanName(name, fallback string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || r == '"' || r == '\\' {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > 40 {
		out = string(r[:40])
	}
	if out == "" {
		return fallback
	}
	return out
}

// cleanCastSubs validates the subtitles a client wants to send: at most
// castMaxSubs, OpenSubtitles ids must be numeric, torrent files must exist
// and be .srt or .vtt. torrentFile returns a file's name by index.
func cleanCastSubs(in []castSub, torrentFile func(int) (string, bool)) []castSub {
	var out []castSub
	for _, s := range in {
		if len(out) >= castMaxSubs {
			break
		}
		lang := castLangTag(s.Lang)
		clean := castSub{Kind: s.Kind, Lang: lang, Name: castCleanName(s.Name, strings.ToUpper(lang))}
		switch s.Kind {
		case "os":
			if _, err := strconv.Atoi(s.ID); err != nil || len(s.ID) > 12 {
				continue
			}
			clean.ID = s.ID
		case "tf":
			name, ok := torrentFile(s.File)
			ext := strings.ToLower(filepath.Ext(name))
			if !ok || (ext != ".srt" && ext != ".vtt") {
				continue
			}
			clean.File = s.File
		default:
			continue
		}
		out = append(out, clean)
	}
	return out
}

// castMasterPlaylist: the video with every subtitle as a rendition, the
// first one on by default. audio < 0 means the default track.
func castMasterPlaylist(subs []castSub, audio int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for i, s := range subs {
		def := "NO"
		if i == 0 {
			def = "YES"
		}
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"%s\",LANGUAGE=\"%s\",DEFAULT=%s,AUTOSELECT=YES,FORCED=NO,URI=\"subs/%d.m3u8\"\n",
			castCleanName(s.Name, "Subtitles"), castLangTag(s.Lang), def, i)
	}
	b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=\"avc1.640029,mp4a.40.2\"", castBandwidth))
	if len(subs) > 0 {
		b.WriteString(",SUBTITLES=\"subs\"")
	}
	b.WriteString("\nindex.m3u8")
	if audio >= 0 {
		fmt.Fprintf(&b, "?audio=%d", audio)
	}
	b.WriteString("\n")
	return b.String()
}

// castSubPlaylist: one WebVTT segment that covers the whole video.
func castSubPlaylist(duration float64, n int) string {
	if duration <= 0 {
		duration = castFallbackRuntime
	}
	return fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:%.3f,\n%d.seg.vtt\n#EXT-X-ENDLIST\n",
		int(math.Ceil(duration)), duration, n)
}

// withTimestampMap adds the header that lines WebVTT up with the MPEG-TS
// clock of an HLS stream (our segments start at 0).
func withTimestampMap(vtt []byte) []byte {
	s := strings.TrimPrefix(string(vtt), "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.Contains(strings.SplitN(s, "\n\n", 2)[0], "X-TIMESTAMP-MAP") {
		return []byte(s)
	}
	head, rest, _ := strings.Cut(s, "\n")
	if !strings.HasPrefix(head, "WEBVTT") {
		return []byte("WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n" + s)
	}
	return []byte(head + "\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n" + rest)
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// castPublicBase is the address receivers use to reach this server.
func castPublicBase(r *http.Request) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("BITPLAY_PUBLIC_URL")), "/"); strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		return v
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || strings.Contains(r.Header.Get("Cf-Visitor"), `"https"`) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// castTokenHandler: POST /api/v1/cast/token {session, file, subs}
// -> {token, base, expiresAt, subs}. subs is what was accepted, in order:
// the N in /hls/subs/N.* is an index into it.
func castTokenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Session string    `json:"session"`
		File    int       `json:"file"`
		Subs    []castSub `json:"subs"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}
	v, ok := sessions.Load(in.Session)
	if !ok {
		respondWithJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	session := v.(*TorrentSession)
	if session.Torrent == nil || session.Torrent.Info() == nil {
		respondWithJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Torrent metadata not available yet"})
		return
	}
	files := session.Torrent.Files()
	if in.File < 0 || in.File >= len(files) {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "File index out of range"})
		return
	}
	session.LastUsed = time.Now()

	subs := cleanCastSubs(in.Subs, func(i int) (string, bool) {
		if i < 0 || i >= len(files) {
			return "", false
		}
		return files[i].DisplayPath(), true
	})
	exp := time.Now().Add(castTokenTTL)
	token, err := signCastToken(castKey(), castClaims{Session: in.Session, File: in.File, Exp: exp.Unix(), Subs: subs})
	if err != nil {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Could not create the link"})
		return
	}
	log.Printf("Cast: link for %s file %d (%d subtitle(s)) by %s", in.Session, in.File, len(subs), currentUser(r).ID)
	if subs == nil {
		subs = []castSub{}
	}
	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"token":     token,
		"base":      castPublicBase(r) + castPathPrefix + token,
		"expiresAt": exp.Unix(),
		"subs":      subs,
	})
}

// castRouter sends /cast/ to castHandler, before any login middleware:
// the signed token in the URL is the authorisation.
func castRouter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, castPathPrefix) {
			castHandler(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func setCastCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Range, Accept, Content-Type")
	h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
	h.Set("Access-Control-Max-Age", "86400")
}

func castHandler(w http.ResponseWriter, r *http.Request) {
	setCastCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, castPathPrefix), "/")
	claims, err := verifyCastToken(castKey(), token, time.Now())
	if err != nil {
		http.Error(w, "This cast link is invalid or has expired", http.StatusForbidden)
		return
	}
	v, ok := sessions.Load(claims.Session)
	if !ok {
		http.Error(w, "The video session is closed, start casting again", http.StatusNotFound)
		return
	}
	session := v.(*TorrentSession)
	session.LastUsed = time.Now()

	switch {
	case rest == "hls/master.m3u8":
		audio := -1
		if a := r.URL.Query().Get("audio"); a != "" {
			if n, err := strconv.Atoi(a); err == nil && n >= 0 && n <= 512 {
				audio = n
			}
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, castMasterPlaylist(claims.Subs, audio))
	case rest == "hls/index.m3u8" || (strings.HasPrefix(rest, "hls/seg") && strings.HasSuffix(rest, ".ts")):
		if session.Torrent == nil || session.Torrent.Info() == nil {
			http.Error(w, "Torrent metadata not available yet", http.StatusServiceUnavailable)
			return
		}
		handleHLS(w, r, session, claims.Session, strconv.Itoa(claims.File), strings.TrimPrefix(rest, "hls/"))
	case strings.HasPrefix(rest, "hls/subs/"):
		serveCastSub(w, r, session, claims, strings.TrimPrefix(rest, "hls/subs/"))
	default:
		http.NotFound(w, r)
	}
}

// castSubtitleVTT returns the WebVTT of one of the token's subtitles.
func castSubtitleVTT(ctx context.Context, session *TorrentSession, s castSub) ([]byte, error) {
	switch s.Kind {
	case "os":
		return fetchSubtitleVTT(snapshotSubtitleConfig(), s.ID)
	case "tf":
		if session.Torrent == nil || session.Torrent.Info() == nil {
			return nil, errors.New("torrent metadata not available yet")
		}
		files := session.Torrent.Files()
		if s.File < 0 || s.File >= len(files) {
			return nil, errors.New("subtitle file out of range")
		}
		reader := files[s.File].NewReader()
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, castMaxSubSize))
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(filepath.Ext(files[s.File].DisplayPath()), ".srt") {
			return convertSRTtoVTT(data), nil
		}
		return data, nil
	}
	return nil, errors.New("unknown subtitle kind")
}

// serveCastSub handles "<N>.m3u8", "<N>.seg.vtt" and "<N>.vtt".
func serveCastSub(w http.ResponseWriter, r *http.Request, session *TorrentSession, claims *castClaims, name string) {
	var numStr, kind string
	switch {
	case strings.HasSuffix(name, ".seg.vtt"):
		numStr, kind = strings.TrimSuffix(name, ".seg.vtt"), "segvtt"
	case strings.HasSuffix(name, ".vtt"):
		numStr, kind = strings.TrimSuffix(name, ".vtt"), "vtt"
	case strings.HasSuffix(name, ".m3u8"):
		numStr, kind = strings.TrimSuffix(name, ".m3u8"), "m3u8"
	default:
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n < 0 || n >= len(claims.Subs) {
		http.NotFound(w, r)
		return
	}

	if kind == "m3u8" {
		duration, err := hlsDuration(r.Context(), session, claims.Session, claims.File)
		if err != nil {
			duration = 0
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, castSubPlaylist(duration, n))
		return
	}

	vtt, err := castSubtitleVTT(r.Context(), session, claims.Subs[n])
	if err != nil {
		log.Printf("Cast: subtitle %d of %s: %v", n, claims.Session, err)
		http.Error(w, "Could not load this subtitle", http.StatusBadGateway)
		return
	}
	if kind == "segvtt" {
		vtt = withTimestampMap(vtt)
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(vtt)
}
