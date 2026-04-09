package bridge

import (
	"net/http"
	"slices"
)

// CORSOptions configures CORS behavior for the bridge HTTP server.
type CORSOptions struct {
	AllowOrigins []string // Allowed origins; default ["*"].
}

// WithCORS wraps an http.Handler with CORS headers.
func WithCORS(h http.Handler, opts CORSOptions) http.Handler {
	origins := opts.AllowOrigins
	if len(origins) == 0 {
		origins = []string{"*"}
	}
	wildcard := len(origins) == 1 && origins[0] == "*"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			matched := false
			if wildcard {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				matched = true
			} else if slices.Contains(origins, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				matched = true
			}
			if matched {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h.ServeHTTP(w, r)
	})
}
