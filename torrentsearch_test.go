package main

import (
	"strings"
	"testing"
)

func TestNormalizeInfoHash(t *testing.T) {
	cases := map[string]string{
		"D7A46713EAEE18C746B3254B7D1492A50FD9D6CE": "d7a46713eaee18c746b3254b7d1492a50fd9d6ce",
		"0000000000000000000000000000000000000000": "",
		"not-a-hash": "",
		// base32 form of the hash above
		"26SGOE7K5YMMORVTEVFX2FESUUH5TVWO": "d7a46713eaee18c746b3254b7d1492a50fd9d6ce",
	}
	for in, want := range cases {
		if got := normalizeInfoHash(in); got != want {
			t.Errorf("normalizeInfoHash(%q) = %q, want %q", in, got, want)
		}
	}
	m := "magnet:?xt=urn:btih:D7A46713EAEE18C746B3254B7D1492A50FD9D6CE&dn=x"
	if got := infoHashFromMagnet(m); got != "d7a46713eaee18c746b3254b7d1492a50fd9d6ce" {
		t.Errorf("infoHashFromMagnet = %q", got)
	}
}

func TestBuildMagnet(t *testing.T) {
	m := buildMagnet("abc", "The Matrix (1999) [1080p]")
	if !strings.HasPrefix(m, "magnet:?xt=urn:btih:abc&dn=The+Matrix+%281999%29+%5B1080p%5D&tr=") {
		t.Errorf("unexpected magnet prefix: %s", m)
	}
	if strings.Count(m, "&tr=") != len(publicSearchTrackers) {
		t.Errorf("expected %d trackers in %s", len(publicSearchTrackers), m)
	}
}

func TestParseYTS(t *testing.T) {
	body := `{"status":"ok","data":{"movie_count":1,"movies":[{"title":"The Matrix","title_long":"The Matrix (1999)","imdb_code":"tt0133093",
	  "torrents":[{"hash":"D7A46713EAEE18C746B3254B7D1492A50FD9D6CE","quality":"1080p","type":"bluray","seeds":100,"peers":5,"size_bytes":1992277407,"date_uploaded_unix":1339543961},
	              {"hash":"bad","quality":"720p","type":"web","seeds":1,"peers":1,"size_bytes":1}]}]}}`
	res, err := parseYTS([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 result (invalid hash dropped), got %d", len(res))
	}
	r := res[0]
	if r.Title != "The Matrix (1999) [1080p bluray]" || r.Seeders != 100 || r.Leechers != 5 ||
		r.SizeBytes != 1992277407 || r.Category != "movies" || r.Indexer != "YTS" || r.Added != 1339543961 {
		t.Errorf("unexpected result %+v", r)
	}
	if infoHashFromMagnet(r.MagnetURL) != r.InfoHash || !r.DirectMagnet {
		t.Errorf("magnet does not carry the info hash: %s", r.MagnetURL)
	}

	if _, err := parseYTS([]byte(`<html>cloudflare</html>`)); err == nil {
		t.Error("expected an error for a non-JSON body")
	}
}

func TestParseTPB(t *testing.T) {
	body := `[
	 {"id":"7349687","name":"The Matrix (1999) 1080p BrRip x264 - YIFY","info_hash":"D7A46713EAEE18C746B3254B7D1492A50FD9D6CE","leechers":"56","seeders":"968","size":"1992277407","added":"1339543961","category":"207","imdb":"tt0133093"},
	 {"id":"82523338","name":"Breaking Bad S02E05 Breakage 2160p","info_hash":"F0DAD5C765FF1C579DB67A6465041896E5B10CFF","leechers":"8","seeders":"34","size":"5938191524","added":"1774129909","category":"212","imdb":"tt0903747"},
	 {"id":"1","name":"Matrix soundtrack","info_hash":"A0DAD5C765FF1C579DB67A6465041896E5B10CFF","leechers":"1","seeders":"9","size":"100","added":"1","category":"101","imdb":""}
	]`
	all, err := parseTPB([]byte(body), "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 video results (audio dropped), got %+v", all)
	}
	if all[0].Category != "movies" || all[0].Seeders != 968 || all[0].imdb != "tt0133093" {
		t.Errorf("unexpected movie row %+v", all[0])
	}
	if all[1].Category != "tv" || all[1].SizeBytes != 5938191524 {
		t.Errorf("unexpected tv row %+v", all[1])
	}

	tv, _ := parseTPB([]byte(body), "tv")
	if len(tv) != 1 || tv[0].Category != "tv" {
		t.Errorf("tv filter: %+v", tv)
	}

	empty := `[{"id":"0","name":"No results returned","info_hash":"0000000000000000000000000000000000000000","leechers":"0","seeders":"0","size":"0","added":"0","category":"0","imdb":""}]`
	if res, _ := parseTPB([]byte(empty), "all"); len(res) != 0 {
		t.Errorf("placeholder row not dropped: %+v", res)
	}
}

func TestParseEZTV(t *testing.T) {
	body := `{"torrents_count":1,"limit":1,"page":1,"torrents":[{"id":1,"hash":"5706a06d5bb428df4a1058c2fdc902372ae51986",
	  "filename":"Archive X S01E06 720p WEB H264-JFF[EZTVx.to].mkv","title":"Archive X S01E06 720p WEB H264-JFF EZTV",
	  "magnet_url":"magnet:?xt=urn:btih:5706a06d5bb428df4a1058c2fdc902372ae51986&dn=Archive+X","imdb_id":"0903747",
	  "seeds":12,"peers":3,"size_bytes":"493030173","date_released_unix":1762000000}]}`
	rows, err := parseEZTV([]byte(body))
	if err != nil || len(rows) != 1 {
		t.Fatalf("parseEZTV: %v %+v", err, rows)
	}
	if string(rows[0].ImdbID) != "0903747" {
		t.Errorf("imdb id = %q", rows[0].ImdbID)
	}
	r, ok := rows[0].toResult()
	if !ok || r.Seeders != 12 || r.SizeBytes != 493030173 || r.Category != "tv" ||
		!strings.HasPrefix(r.MagnetURL, "magnet:?xt=urn:btih:5706a06d") {
		t.Errorf("unexpected result %+v", r)
	}
	if !matchesAllTokens(rows[0].Title+" "+rows[0].Filename, queryTokens("archive x s01e06")) {
		t.Error("expected tokens to match")
	}
	if matchesAllTokens(rows[0].Title, queryTokens("archive x s01e07")) {
		t.Error("unexpected token match")
	}

	for in, want := range map[string]string{"tt0903747": "0903747", "0": "", "": "", "abc": ""} {
		if got := eztvImdbID(in); got != want {
			t.Errorf("eztvImdbID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeResults(t *testing.T) {
	a := []searchResult{
		{Title: "A low", InfoHash: "h1", Seeders: 5, Indexer: "YTS"},
		{Title: "B", InfoHash: "h2", Seeders: 50, Added: 1},
		{Title: "http only", Seeders: 1},
	}
	b := []searchResult{
		{Title: "A high", InfoHash: "h1", Seeders: 500, Indexer: "TPB"},
		{Title: "C", InfoHash: "h3", Seeders: 50, Added: 2},
	}
	got := mergeResults(a, b)
	var titles []string
	for _, r := range got {
		titles = append(titles, r.Title)
	}
	want := "A high,C,B,http only"
	if strings.Join(titles, ",") != want {
		t.Errorf("merge order = %v, want %s", titles, want)
	}
}

func TestParseHumanSize(t *testing.T) {
	for in, want := range map[string]int64{"1.00 GB": 1 << 30, "512 B": 512, "2.50 MB": 2621440, "": 0, "huge": 0} {
		if got := parseHumanSize(in); got != want {
			t.Errorf("parseHumanSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestPublicSourcesSettings(t *testing.T) {
	settingsMutex.Lock()
	saved := currentSettings
	currentSettings.DisablePublicSearch = false
	currentSettings.DisabledSearchSources = []string{"eztv"}
	settingsMutex.Unlock()
	defer func() {
		settingsMutex.Lock()
		currentSettings = saved
		settingsMutex.Unlock()
	}()

	var ids []string
	for _, s := range enabledPublicSources() {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "yts,tpb" {
		t.Errorf("enabled sources = %v", ids)
	}

	settingsMutex.Lock()
	currentSettings.DisablePublicSearch = true
	settingsMutex.Unlock()
	if len(enabledPublicSources()) != 0 {
		t.Error("expected no sources when public search is disabled")
	}
}
