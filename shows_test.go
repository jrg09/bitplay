package main

import "testing"

func testSeries() *seriesInfo {
	return &seriesInfo{
		Meta: &mediaMeta{IMDb: "tt0903747", Title: "Breaking Bad", Type: "series"},
		Seasons: []seriesSeason{
			{Season: 1, Episodes: []seriesEpisode{{Episode: 1, Name: "Pilot", Aired: true}, {Episode: 2, Name: "Cat", Aired: true}}},
			{Season: 2, Episodes: []seriesEpisode{{Episode: 1, Name: "737", Aired: true}, {Episode: 2, Name: "Grilled", Aired: false}}},
			{Season: 0, Episodes: []seriesEpisode{{Episode: 1, Name: "Special", Aired: true}}},
		},
	}
}

func TestFindNextEpisode(t *testing.T) {
	info := testSeries()
	show := func(last string, eps map[string]*episodeProgress) *showProgress {
		return &showProgress{LastEpisode: last, Episodes: eps}
	}
	cases := []struct {
		name string
		sp   *showProgress
		want *nextEpisode
	}{
		{"nothing watched", nil, &nextEpisode{Season: 1, Episode: 1, Name: "Pilot", Aired: true}},
		{"half watched", show("S01E02", map[string]*episodeProgress{"S01E02": {Position: 300, Duration: 2700}}),
			&nextEpisode{Season: 1, Episode: 2, Name: "Cat", Aired: true, Resume: true}},
		{"after the last one", show("S01E02", map[string]*episodeProgress{"S01E02": {WatchedAt: 1}}),
			&nextEpisode{Season: 2, Episode: 1, Name: "737", Aired: true}},
		{"skips watched", show("S01E01", map[string]*episodeProgress{"S01E01": {WatchedAt: 1}, "S01E02": {WatchedAt: 1}}),
			&nextEpisode{Season: 2, Episode: 1, Name: "737", Aired: true}},
		{"not aired yet", show("S02E01", map[string]*episodeProgress{"S02E01": {WatchedAt: 1}}),
			&nextEpisode{Season: 2, Episode: 2, Name: "Grilled", Aired: false}},
		{"last one unmarked", show("S01E02", map[string]*episodeProgress{"S01E02": {}}),
			&nextEpisode{Season: 1, Episode: 2, Name: "Cat", Aired: true}},
		{"all done", show("S02E02", map[string]*episodeProgress{"S02E02": {WatchedAt: 1}}), nil},
		{"special last: restart from first unwatched", show("S00E01", map[string]*episodeProgress{"S00E01": {WatchedAt: 1}}),
			&nextEpisode{Season: 1, Episode: 1, Name: "Pilot", Aired: true}},
	}
	for _, c := range cases {
		got := findNextEpisode(info, c.sp)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestShowRecordProgress(t *testing.T) {
	lib := &userLibrary{Entries: map[string]*libraryEntry{}, Shows: map[string]*showProgress{}}
	ref := &seriesRef{IMDb: "tt0903747", Title: "Breaking Bad", Poster: "https://img/p.jpg", Season: 1, Episode: 2}
	e := &libraryEntry{InfoHash: "aa", FileIndex: 3, FileName: "x.mkv", Position: 600, Duration: 2700}
	showRecordProgressLocked(lib, ref, e)
	sp := lib.Shows["tt0903747"]
	if sp == nil || sp.LastEpisode != "S01E02" || sp.Title != "Breaking Bad" || sp.Poster != "https://img/p.jpg" {
		t.Fatalf("show = %+v", sp)
	}
	ep := sp.Episodes["S01E02"]
	if ep.Position != 600 || ep.InfoHash != "aa" || ep.FileIndex != 3 || ep.WatchedAt != 0 {
		t.Errorf("episode = %+v", ep)
	}

	// finished
	e.Position, e.WatchedAt = 2650, 123
	showRecordProgressLocked(lib, ref, e)
	if ep.WatchedAt != 123 {
		t.Errorf("watchedAt = %d, want 123", ep.WatchedAt)
	}
	// rewatching from the start clears the mark
	e.Position, e.WatchedAt = 30, 0
	showRecordProgressLocked(lib, ref, e)
	if ep.WatchedAt != 0 {
		t.Error("rewatch should clear the watched mark")
	}

	// a file named like an episode of a tracked show is linked without ref
	other := &libraryEntry{InfoHash: "bb", FileName: "Breaking.Bad.S02E01.720p.mkv", Position: 10, Duration: 2700}
	showRecordProgressLocked(lib, nil, other)
	if sp.Episodes["S02E01"] == nil || sp.LastEpisode != "S02E01" {
		t.Errorf("name fallback did not link: %+v", sp.Episodes)
	}
	// unknown shows and season packs are ignored
	showRecordProgressLocked(lib, nil, &libraryEntry{FileName: "Other.Show.S01E01.mkv"})
	showRecordProgressLocked(lib, nil, &libraryEntry{FileName: "Breaking.Bad.S03.COMPLETE.mkv"})
	if len(lib.Shows) != 1 || len(sp.Episodes) != 2 {
		t.Errorf("unexpected shows/episodes: %d / %d", len(lib.Shows), len(sp.Episodes))
	}
	if links := showLinkedFilesLocked(lib); links["aa/3"] != "tt0903747" || links["bb/0"] != "tt0903747" {
		t.Errorf("links = %v", links)
	}
}

func TestEpisodeKeys(t *testing.T) {
	if k := episodeKey(2, 5); k != "S02E05" {
		t.Errorf("episodeKey = %s", k)
	}
	if s, e, ok := parseEpisodeKey("S10E105"); !ok || s != 10 || e != 105 {
		t.Errorf("parseEpisodeKey = %d %d %v", s, e, ok)
	}
	if !episodeLess(1, 9, 2, 1) || !episodeLess(5, 1, 0, 1) || episodeLess(0, 1, 1, 1) {
		t.Error("episodeLess order wrong")
	}
}
