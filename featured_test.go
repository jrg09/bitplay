package main

import "testing"

func TestFeaturedGroups(t *testing.T) {
	var in []resultGroup
	for i := 0; i < featuredMaxTitles+5; i++ {
		in = append(in, resultGroup{Key: "tt" + string(rune('a'+i%26)), Meta: &mediaMeta{Title: "x"}})
	}
	in = append([]resultGroup{{Key: "other"}}, in...)
	out := featuredGroups(in)
	if len(out) != featuredMaxTitles {
		t.Fatalf("expected %d titles, got %d", featuredMaxTitles, len(out))
	}
	for _, g := range out {
		if g.Meta == nil {
			t.Fatal("unidentified groups must be dropped")
		}
	}
}

func TestFeaturedListIDs(t *testing.T) {
	want := []string{"popular", "recent", "top-movies", "top-tv"}
	if len(featuredLists) != len(want) {
		t.Fatalf("lists: %+v", featuredLists)
	}
	for i, id := range want {
		if featuredLists[i].ID != id || featuredLists[i].fetch == nil {
			t.Errorf("list %d = %+v, want %s", i, featuredLists[i], id)
		}
	}
}
