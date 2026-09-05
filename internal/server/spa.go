package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
)

// spaHandler serves static assets from uiFS and falls back to index.html for
// any unknown path, so client-side routing works.
func spaHandler(uiFS fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(uiFS))
	return func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			serveIndex(w, uiFS)
			return
		}
		if f, err := uiFS.Open(p); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w, uiFS)
	}
}

func serveIndex(w http.ResponseWriter, uiFS fs.FS) {
	b, err := fs.ReadFile(uiFS, "index.html")
	if err != nil {
		http.Error(w, "ui not built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// orEmpty returns s, or an empty slice when s is nil, so a list field marshals as
// [] rather than null. Encoding "no elements" as null makes the field's type
// depend on its length: every client then needs a null check on a value it was
// told is a list, and the one that forgets fails at the first list operation
// instead of rendering nothing. Use it on any slice that leaves through writeJSON
// and can legitimately be empty.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
