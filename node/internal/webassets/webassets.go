// Package webassets serves Mira's embedded administrator console.
package webassets

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; worker-src 'self'; manifest-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; frame-src 'self' blob:; connect-src 'self' ws: wss:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// files contains the complete production Web UI. Third-party browser modules
// are checked in under web/vendor so serving the UI never depends on a Node.js
// installation or a runtime node_modules directory.
//
//go:embed web/* web/icons/* web/vendor/*
var files embed.FS

type assetSpec struct {
	file        string
	contentType string
}

type asset struct {
	payload     []byte
	contentType string
	etag        string
}

var specs = map[string]assetSpec{
	"/trace-diagrams.js":         {"web/trace-diagrams.js", "text/javascript; charset=utf-8"},
	"/vendor/mermaid.js":         {"web/vendor/mermaid.js", "text/javascript; charset=utf-8"},
	"/":                          {"web/index.html", "text/html; charset=utf-8"},
	"/app.js":                    {"web/app.js", "text/javascript; charset=utf-8"},
	"/thread-title.js":           {"web/thread-title.js", "text/javascript; charset=utf-8"},
	"/thread-list.js":            {"web/thread-list.js", "text/javascript; charset=utf-8"},
	"/thread-usage.js":           {"web/thread-usage.js", "text/javascript; charset=utf-8"},
	"/thread-model.js":           {"web/thread-model.js", "text/javascript; charset=utf-8"},
	"/account-history.js":        {"web/account-history.js", "text/javascript; charset=utf-8"},
	"/account-quota.js":          {"web/account-quota.js", "text/javascript; charset=utf-8"},
	"/account-status.js":         {"web/account-status.js", "text/javascript; charset=utf-8"},
	"/trace-activity.js":         {"web/trace-activity.js", "text/javascript; charset=utf-8"},
	"/trace-images.js":           {"web/trace-images.js", "text/javascript; charset=utf-8"},
	"/composer-drafts.js":        {"web/composer-drafts.js", "text/javascript; charset=utf-8"},
	"/conversation-progress.js":  {"web/conversation-progress.js", "text/javascript; charset=utf-8"},
	"/theme.js":                  {"web/theme.js", "text/javascript; charset=utf-8"},
	"/styles.css":                {"web/styles.css", "text/css; charset=utf-8"},
	"/pwa.js":                    {"web/pwa.js", "text/javascript; charset=utf-8"},
	"/service-worker.js":         {"web/service-worker.js", "text/javascript; charset=utf-8"},
	"/manifest.webmanifest":      {"web/manifest.webmanifest", "application/manifest+json; charset=utf-8"},
	"/offline.html":              {"web/offline.html", "text/html; charset=utf-8"},
	"/offline.css":               {"web/offline.css", "text/css; charset=utf-8"},
	"/offline.js":                {"web/offline.js", "text/javascript; charset=utf-8"},
	"/icons/mira.svg":            {"web/icons/mira.svg", "image/svg+xml"},
	"/icons/mira-192.png":        {"web/icons/mira-192.png", "image/png"},
	"/icons/mira-512.png":        {"web/icons/mira-512.png", "image/png"},
	"/vendor/xterm.js":           {"web/vendor/xterm.js", "text/javascript; charset=utf-8"},
	"/vendor/xterm-addon-fit.js": {"web/vendor/xterm-addon-fit.js", "text/javascript; charset=utf-8"},
	"/vendor/xterm.css":          {"web/vendor/xterm.css", "text/css; charset=utf-8"},
	"/vendor/marked.js":          {"web/vendor/marked.js", "text/javascript; charset=utf-8"},
	"/vendor/dompurify.js":       {"web/vendor/dompurify.js", "text/javascript; charset=utf-8"},
}

var assets = mustLoadAssets()

func mustLoadAssets() map[string]asset {
	loaded := make(map[string]asset, len(specs))
	for route, spec := range specs {
		payload, err := files.ReadFile(spec.file)
		if err != nil {
			panic(fmt.Sprintf("load embedded Web asset %s: %v", spec.file, err))
		}
		if route == "/service-worker.js" {
			payload = serviceWorkerWithOfflineVersion(payload)
		}
		digest := sha256.Sum256(payload)
		loaded[route] = asset{
			payload:     payload,
			contentType: spec.contentType,
			etag:        `"` + hex.EncodeToString(digest[:]) + `"`,
		}
	}
	return loaded
}

func serviceWorkerWithOfflineVersion(payload []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write(payload)
	for _, file := range []string{"web/offline.html", "web/offline.css", "web/offline.js", "web/icons/mira.svg"} {
		body, err := files.ReadFile(file)
		if err != nil {
			panic(fmt.Sprintf("load embedded service worker dependency %s: %v", file, err))
		}
		_, _ = hash.Write(body)
	}
	version := hex.EncodeToString(hash.Sum(nil))[:20]
	return bytes.Replace(payload, []byte("__MIRA_OFFLINE_VERSION__"), []byte(version), 1)
}

// Serve writes an embedded Web response and reports whether it handled the
// request. Only GET and HEAD requests for the explicit public asset set are
// handled, allowing a Server router to call Serve before its API routes.
func Serve(response http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return false
	}
	entry, ok := assets[request.URL.EscapedPath()]
	if !ok {
		return false
	}

	header := response.Header()
	header.Set("Content-Type", entry.contentType)
	header.Set("Cache-Control", "no-cache")
	header.Set("ETag", entry.etag)
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	if request.URL.EscapedPath() == "/service-worker.js" {
		header.Set("Service-Worker-Allowed", "/")
	}

	if matchesETag(request.Header.Get("If-None-Match"), entry.etag) {
		response.WriteHeader(http.StatusNotModified)
		return true
	}
	header.Set("Content-Length", fmt.Sprintf("%d", len(entry.payload)))
	response.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = response.Write(entry.payload)
	}
	return true
}

func matchesETag(header string, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// NewHandler returns a standalone handler for the administrator console.
// Unknown paths and non-GET methods receive the standard HTTP 404 response.
func NewHandler() http.Handler {
	return Wrap(http.NotFoundHandler())
}

// Wrap serves embedded Web assets before delegating all other requests to next.
func Wrap(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !Serve(response, request) {
			next.ServeHTTP(response, request)
		}
	})
}
