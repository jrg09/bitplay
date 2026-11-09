package main

// Static files. Behind a CDN (Cloudflare) or a browser cache, a stale
// app JS next to a fresh index.html leaves the page half working, so:
//   - index.html is generated per request and links the files that change
//     with the app (index.js, app.css, output.css) with a content hash
//     (?v=...), so a new build is always a new URL;
//   - everything is sent with Cache-Control: no-store, which CDNs also
//     honour by not caching it or rewriting its browser TTL.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
)

// versionedAssets are the first-party files linked from index.html.
var versionedAssets = []string{"assets/index.js", "assets/app.css", "assets/output.css"}

// versionedIndexHTML returns index.html with ?v=<hash of the file> added to
// the links of versionedAssets.
func versionedIndexHTML(clientDir string) ([]byte, error) {
	html, err := os.ReadFile(filepath.Join(clientDir, "index.html"))
	if err != nil {
		return nil, err
	}
	for _, asset := range versionedAssets {
		data, err := os.ReadFile(filepath.Join(clientDir, filepath.FromSlash(asset)))
		if err != nil {
			continue // missing file: leave its link as it is
		}
		sum := sha256.Sum256(data)
		version := hex.EncodeToString(sum[:4])
		html = bytes.ReplaceAll(html, []byte(asset+`"`), []byte(asset+"?v="+version+`"`))
	}
	return html, nil
}

// staticHandler serves ./client with the cache rules above.
func staticHandler(clientDir string) http.Handler {
	fileServer := http.FileServer(http.Dir(clientDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			html, err := versionedIndexHTML(clientDir)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.Method != http.MethodHead {
				w.Write(html)
			}
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
