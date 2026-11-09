package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTraitsOf(t *testing.T) {
	cases := []struct {
		release            string
		group, source, res string
	}{
		{"Resident.Evil.2026.1080p.AMZN.WEB-DL.DDP5.1.Atmos.H.264-Kitsune", "kitsune", "web", "1080p"},
		{"Movie 2026 2160p BluRay x265-RARBG", "rarbg", "bluray", "2160p"},
		{"Movie.2026.720p.WEBRip.x264-YTS", "yts", "webrip", "720p"},
		{"Movie.2026.1080p.BDRip.x264-GRP", "grp", "bluray", "1080p"},
		{"Movie 2026 4K WEB-DL", "", "web", "2160p"}, // "-DL" is not a group
		{"Show S01E02 HDTV x264-LOL[eztv]", "lol", "hdtv", ""},
		{"Plain title", "", "", ""},
	}
	for _, c := range cases {
		got := traitsOf(c.release)
		if got.group != c.group || got.source != c.source || got.res != c.res {
			t.Errorf("traitsOf(%q) = %+v, want group=%q source=%q res=%q", c.release, got, c.group, c.source, c.res)
		}
	}
}

func TestSubtitleMatchLevel(t *testing.T) {
	torrent := "Resident.Evil.2026.1080p.AMZN.WEB-DL.DDP5.1.H.264-Kitsune"
	cases := []struct {
		name string
		subs []string
		want string
	}{
		{"no subtitles", nil, "none"},
		{"same group wins over other rows", []string{"Resident Evil 2026 720p BluRay", "Resident.Evil.2026.1080p.WEB-DL.H.264-Kitsune"}, "exact"},
		{"same source and resolution", []string{"Resident.Evil.2026.1080p.WEB-DL.DDP5.1.H.264-KyoGo"}, "similar"},
		{"different version", []string{"Resident Evil 2026 720p BluRay x264-YTS"}, "other"},
		{"source matches but not resolution", []string{"Resident.Evil.2026.2160p.WEB-DL-KyoGo"}, "other"},
		{"group must be a whole token", []string{"Resident.Evil.2026.720p.BluRay-Kitsunetsuki"}, "other"},
	}
	for _, c := range cases {
		if got, _ := subtitleMatchLevel(torrent, c.subs); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// no group, no source info: any subtitle is just "other"
	if got, best := subtitleMatchLevel("Some Title 2026", []string{"A", "B"}); got != "other" || best != "A" {
		t.Errorf("got %q %q", got, best)
	}
}

func TestSubAvailKeyFor(t *testing.T) {
	cases := []struct {
		typ, release string
		want         subAvailKey
		ok           bool
	}{
		{"movie", "Anything 2026 1080p", subAvailKey{IMDb: "tt1"}, true},
		{"series", "Show.S02E05.1080p.WEB-DL-GRP", subAvailKey{"tt1", 2, 5}, true},
		{"series", "Show 2x05 720p", subAvailKey{"tt1", 2, 5}, true},
		{"series", "Show.S03.COMPLETE.1080p.BluRay", subAvailKey{"tt1", 3, 0}, true},
		{"series", "Show Season 1-4 Complete", subAvailKey{IMDb: "tt1"}, true},
		{"series", "Show 2026 1080p", subAvailKey{}, false}, // unknown episode
	}
	for _, c := range cases {
		got, ok := subAvailKeyFor("tt1", c.typ, c.release)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("subAvailKeyFor(%s, %q) = %+v, %v; want %+v, %v", c.typ, c.release, got, ok, c.want, c.ok)
		}
	}
}

func TestSubAvailQuery(t *testing.T) {
	// the OpenSubtitles API redirects non-canonical URLs, so keys must stay
	// sorted and values lowercase: Encode() sorts, check the values
	movie := subAvailQuery(subAvailKey{IMDb: "tt0903747"}, "movie").Encode()
	if movie != "imdb_id=0903747&languages=es%2Cea%2Csp&type=movie" {
		t.Errorf("movie query = %s", movie)
	}
	ep := subAvailQuery(subAvailKey{"tt0903747", 2, 5}, "series").Encode()
	if ep != "episode_number=5&languages=es%2Cea%2Csp&parent_imdb_id=0903747&season_number=2&type=episode" {
		t.Errorf("episode query = %s", ep)
	}
	season := subAvailQuery(subAvailKey{"tt0903747", 2, 0}, "series").Encode()
	if strings.Contains(season, "episode_number") || !strings.Contains(season, "season_number=2") {
		t.Errorf("season query = %s", season)
	}
}

func TestSubAvailabilityHandler(t *testing.T) {
	settingsMutex.Lock()
	saved := currentSettings
	currentSettings.EnableSubtitles, currentSettings.OpenSubtitlesApiKey = true, "key"
	settingsMutex.Unlock()
	defer func() { settingsMutex.Lock(); currentSettings = saved; settingsMutex.Unlock() }()

	// seed the cache so no request leaves the process
	subAvailCachePut("series|tt0903747|2|5", []string{"Show.S02E05.1080p.WEB-DL-GRP"})
	subAvailCachePut("series|tt0903747|3|0", []string{})
	defer func() {
		subAvail.Lock()
		subAvail.m = map[string]subAvailEntry{}
		subAvail.Unlock()
	}()

	post := func(body string) (int, map[string]interface{}) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/subtitles/availability", strings.NewReader(body))
		rec := httptest.NewRecorder()
		subtitleAvailabilityHandler(rec, req)
		var out map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := post(`{"imdb":"tt0903747","type":"series","releases":[
		"Show.S02E05.720p.HDTV-GRP", "Show.S02E05.2160p.BluRay-XYZ", "Show.S03.COMPLETE.1080p", "Show 2026 no episode"]}`)
	if code != 200 || out["enabled"] != true {
		t.Fatalf("code %d out %v", code, out)
	}
	res := out["results"].(map[string]interface{})
	level := func(name string) string {
		if r, ok := res[name].(map[string]interface{}); ok {
			return r["level"].(string)
		}
		return "(absent)"
	}
	if level("Show.S02E05.720p.HDTV-GRP") != "exact" { // same group
		t.Errorf("same group: %v", res)
	}
	if level("Show.S02E05.2160p.BluRay-XYZ") != "other" {
		t.Errorf("other version: %v", res)
	}
	if level("Show.S03.COMPLETE.1080p") != "none" {
		t.Errorf("season without subtitles: %v", res)
	}
	if level("Show 2026 no episode") != "(absent)" {
		t.Errorf("unidentifiable release must be left out: %v", res)
	}

	if code, _ := post(`{"imdb":"nope","releases":[]}`); code != 400 {
		t.Errorf("bad imdb: code %d", code)
	}

	settingsMutex.Lock()
	currentSettings.OpenSubtitlesApiKey = ""
	settingsMutex.Unlock()
	if _, out := post(`{"imdb":"tt0903747","type":"series","releases":["x"]}`); out["enabled"] != false {
		t.Errorf("without an API key the feature must be off: %v", out)
	}
}
