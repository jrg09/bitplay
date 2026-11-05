package main

import (
	"testing"
	"time"
)

func TestClassifySeriesName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
		want seriesRelease
	}{
		{"The.Bear.S02E05.1080p.WEB.h264-ETHEL", true, seriesRelease{SeasonFrom: 2, SeasonTo: 2, Episode: 5}},
		{"The Bear S05E08 The Original Beef 720p DSNP WEB-DL", true, seriesRelease{SeasonFrom: 5, SeasonTo: 5, Episode: 8}},
		{"Show.2x05.HDTV", true, seriesRelease{SeasonFrom: 2, SeasonTo: 2, Episode: 5}},
		{"Show.S01E01-E03.720p", true, seriesRelease{Pack: true, SeasonFrom: 1, SeasonTo: 1}},
		{"Show.S01E01E02.720p", true, seriesRelease{Pack: true, SeasonFrom: 1, SeasonTo: 1}},
		{"Show S01E10 1080p x264", true, seriesRelease{SeasonFrom: 1, SeasonTo: 1, Episode: 10}},
		{"The.Bear.S02.COMPLETE.1080p.HULU.WEB.h264-EDITH[TGx]", true, seriesRelease{Pack: true, SeasonFrom: 2, SeasonTo: 2}},
		{"The Bear (2022) Season 2 S02 (1080p HULU WEB-DL x265", true, seriesRelease{Pack: true, SeasonFrom: 2, SeasonTo: 2}},
		{"Breaking Bad Season 2 Complete 720p.BRrip.Sujaidr", true, seriesRelease{Pack: true, SeasonFrom: 2, SeasonTo: 2}},
		{"Breaking Bad S01-S05 Complete 720p BlaRay x265-BMF", true, seriesRelease{Pack: true, SeasonFrom: 1, SeasonTo: 5}},
		{"Breaking.Bad.Complete.S01-S05.1080p.10bit.BluRay", true, seriesRelease{Pack: true, SeasonFrom: 1, SeasonTo: 5}},
		{"Friends Seasons 1-10 1080p", true, seriesRelease{Pack: true, SeasonFrom: 1, SeasonTo: 10}},
		{"Some Show Complete Series 720p", true, seriesRelease{Pack: true}},
		{"Breaking Bad - Complete Season + Extras - BluRay 1080p", false, seriesRelease{}},
		{"Some Movie 2024 1080p", false, seriesRelease{}},
	}
	for _, c := range cases {
		got, ok := classifySeriesName(c.name)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("classifySeriesName(%q) = %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestBelongsToSeries(t *testing.T) {
	const bear = "tt14452776"
	cases := []struct {
		r    searchResult
		want bool
	}{
		{searchResult{Title: "The.Bear.S02E05.1080p"}, true},
		{searchResult{Title: "The Bear (2022) Season 2 S02"}, true},
		{searchResult{Title: "Bear S02E05 720p"}, true}, // "the" dropped
		{searchResult{Title: "The Yogi Bear Show 1958 Season 1"}, false},
		{searchResult{Title: "BJ and The Bear 1979 Season 1", imdb: "tt0078564"}, false},
		{searchResult{Title: "The.Bear.S02.COMPLETE", imdb: bear}, true},
		{searchResult{Title: "The.Bear.S02.COMPLETE", imdb: "tt0000001"}, false}, // known id wins
		{searchResult{Title: "Breaking Brad S01E01"}, false},                     // EZTV wrong id
		{searchResult{Title: "The Bearded S01E01"}, false},
	}
	for _, c := range cases {
		if got := belongsToSeries(c.r, bear, "The Bear"); got != c.want {
			t.Errorf("belongsToSeries(%q, imdb %q) = %v, want %v", c.r.Title, c.r.imdb, got, c.want)
		}
	}
}

func TestRankSeriesResults(t *testing.T) {
	list := []searchResult{
		{Title: "Show S01E01 720p", Seeders: 500},
		{Title: "Show S01E01 1080p", Seeders: 40},
		{Title: "Show S01E01 1080p LATINO", Seeders: 15},
		{Title: "Show S01E01 1080p Dual", Seeders: 1}, // too few seeders
		{Title: "Show S01E01 2160p", Seeders: 900},
		{Title: "Show S01E01 1080p", Seeders: 80},
		{Title: "Show S01E01 1080p x265 LATINO", Seeders: 999},
		{Title: "Show.S01E01.1080p.HEVC", Seeders: 998},
	}
	rankSeriesResults(list)
	want := []string{
		"Show S01E01 1080p LATINO", "Show S01E01 1080p", "Show S01E01 1080p",
		"Show S01E01 720p", "Show S01E01 2160p",
		"Show S01E01 1080p x265 LATINO", "Show.S01E01.1080p.HEVC", "Show S01E01 1080p Dual",
	}
	for i, w := range want {
		if list[i].Title != w {
			t.Fatalf("rank[%d] = %q (%d seeders), want %q; full: %+v", i, list[i].Title, list[i].Seeders, w, list)
		}
	}
	if list[1].Seeders != 80 {
		t.Errorf("same quality should sort by seeders, got %d first", list[1].Seeders)
	}
}

func TestBuildSeriesTorrentList(t *testing.T) {
	const id = "tt14452776"
	candidates := []searchResult{
		{Title: "The.Bear.S02E05.1080p", InfoHash: "a", Seeders: 50},
		{Title: "The.Bear.S02E05.720p", InfoHash: "b", Seeders: 90},
		{Title: "The.Bear.S02E06.1080p", InfoHash: "c", Seeders: 1},
		{Title: "The.Bear.S03E01.1080p", InfoHash: "d", Seeders: 70},
		{Title: "The.Bear.S02.COMPLETE.1080p", InfoHash: "e", Seeders: 500, imdb: id},
		{Title: "The.Bear.S01-S03.720p", InfoHash: "f", Seeders: 20},
		{Title: "Breaking Brad S02E05", InfoHash: "g", Seeders: 999},
		{Title: "The.Bear.S02E05.1080p", InfoHash: "a", Seeders: 60}, // duplicate hash
	}
	l := buildSeriesTorrentList(candidates, id, "The Bear", 2, 0)
	if len(l.Episodes["5"]) != 2 || l.Episodes["5"][0].InfoHash != "a" || l.Episodes["5"][0].Seeders != 60 {
		t.Errorf("episode 5 = %+v", l.Episodes["5"])
	}
	if len(l.Packs) != 2 || l.Packs[0].InfoHash != "e" {
		t.Errorf("packs = %+v", l.Packs)
	}
	if _, ok := l.Episodes["1"]; ok {
		t.Error("season 3 episode leaked into season 2")
	}
	if b := l.Best["5"]; b == nil || b.Pack || b.Torrent.InfoHash != "a" {
		t.Errorf("best for E05 = %+v, want the 1080p episode", b)
	}
	if b := l.Best["6"]; b == nil || !b.Pack || b.Torrent.InfoHash != "e" {
		t.Errorf("best for E06 = %+v, want the season pack (episode has 1 seeder)", b)
	}

	// an episode with no torrent of its own still gets the pack
	one := buildSeriesTorrentList(candidates, id, "The Bear", 2, 9)
	if len(one.Episodes) != 0 || one.Best["9"] == nil || !one.Best["9"].Pack {
		t.Errorf("episode 9: %+v / best %+v", one.Episodes, one.Best["9"])
	}
}

func TestParseSeriesMeta(t *testing.T) {
	body := []byte(`{"meta":{"imdb_id":"tt0903747","type":"series","name":"Breaking Bad","releaseInfo":"2008–2013",
		"poster":"https://img/p.jpg","imdbRating":"9.5","videos":[
		{"id":"tt0903747:1:2","season":1,"episode":2,"name":"Cat's in the Bag...","released":"2008-01-27T00:00:00.000Z","thumbnail":"https://img/1-2.jpg","overview":"o"},
		{"id":"tt0903747:1:1","season":1,"episode":1,"title":"Pilot","released":"2008-01-20T00:00:00.000Z","thumbnail":"http://insecure/x.jpg","description":"d"},
		{"id":"tt0903747:0:1","season":0,"episode":1,"name":"Special","released":"2009-02-17T00:00:00.000Z"},
		{"id":"tt0903747:2:1","season":2,"episode":1,"name":"Future","released":"2030-01-01T00:00:00.000Z"},
		{"id":"bad","season":2,"episode":0,"name":"No number"}
	]}}`)
	info, err := parseSeriesMeta(body, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if info.Meta.Title != "Breaking Bad" || info.Meta.Type != "series" {
		t.Errorf("meta = %+v", info.Meta)
	}
	var order []int
	for _, s := range info.Seasons {
		order = append(order, s.Season)
	}
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 0 {
		t.Fatalf("season order = %v, want [1 2 0]", order)
	}
	s1 := info.Seasons[0].Episodes
	if len(s1) != 2 || s1[0].Episode != 1 || s1[0].Name != "Pilot" || s1[0].Overview != "d" || s1[0].Thumbnail != "" {
		t.Errorf("season 1 = %+v", s1)
	}
	if s1[1].Released != "2008-01-27" || !s1[1].Aired || s1[1].Thumbnail != "https://img/1-2.jpg" {
		t.Errorf("S01E02 = %+v", s1[1])
	}
	if s2 := info.Seasons[1].Episodes; len(s2) != 1 || s2[0].Aired {
		t.Errorf("season 2 = %+v (future episode must not be aired)", s2)
	}
}
