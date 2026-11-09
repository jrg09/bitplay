package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeClient(t *testing.T, js, css string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "client")
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"index.html": `<link rel="stylesheet" href="assets/output.css" />` +
			`<link rel="stylesheet" href="assets/app.css" />` +
			`<script src="assets/video.min.js"></script>` +
			`<script type="module" src="assets/index.js"></script>`,
		"assets/index.js":     js,
		"assets/app.css":      css,
		"assets/output.css":   "out",
		"assets/video.min.js": "vendor",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestIndexLinksCarryContentHash(t *testing.T) {
	dir := writeClient(t, "console.log(1)", "a{}")
	h := staticHandler(dir)

	first := get(h, "/")
	if first.Code != 200 || !strings.HasPrefix(first.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("code %d type %q", first.Code, first.Header().Get("Content-Type"))
	}
	body := first.Body.String()
	for _, asset := range []string{"assets/index.js", "assets/app.css", "assets/output.css"} {
		if !regexp.MustCompile(regexp.QuoteMeta(asset) + `\?v=[0-9a-f]{8}"`).MatchString(body) {
			t.Errorf("%s has no version in:\n%s", asset, body)
		}
	}
	if strings.Contains(body, "video.min.js?v=") {
		t.Error("vendor files must not be versioned")
	}

	// same content, same URL; new content, new URL
	if again := get(h, "/index.html").Body.String(); again != body {
		t.Error("the version must be stable while the files don't change")
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "index.js"), []byte("console.log(2)"), 0644); err != nil {
		t.Fatal(err)
	}
	changed := get(h, "/").Body.String()
	version := func(s string) string {
		return regexp.MustCompile(`index\.js\?v=([0-9a-f]{8})`).FindStringSubmatch(s)[1]
	}
	if version(changed) == version(body) {
		t.Error("a changed index.js must get a new version")
	}
	cssVersion := func(s string) string {
		return regexp.MustCompile(`app\.css\?v=([0-9a-f]{8})`).FindStringSubmatch(s)[1]
	}
	if cssVersion(changed) != cssVersion(body) {
		t.Error("an unchanged file must keep its version")
	}
}

func TestStaticFilesAreNotCacheable(t *testing.T) {
	h := staticHandler(writeClient(t, "js", "css"))
	for _, path := range []string{"/", "/index.html", "/assets/index.js", "/assets/app.css?v=abc"} {
		rec := get(h, path)
		if rec.Code != 200 {
			t.Errorf("%s: code %d", path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, cc)
		}
	}
	if rec := get(h, "/assets/index.js"); rec.Body.String() != "js" {
		t.Errorf("asset body = %q", rec.Body.String())
	}
	if rec := get(h, "/nope.js"); rec.Code != 404 {
		t.Errorf("missing file: code %d", rec.Code)
	}
}
