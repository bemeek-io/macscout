package api

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spaHandler serves the built React bundle, falling back to index.html so
// client-side routes survive a hard refresh.
func (s *Server) spaHandler() http.Handler {
	files := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(s.static, name); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "static asset unavailable", http.StatusInternalServerError)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			name = "index.html"
		}
		// Hashed bundle assets are immutable; index.html must not be.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
