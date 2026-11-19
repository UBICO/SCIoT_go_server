package dashboard

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// built è una dist dopo npm run build, ridotta all'essenziale.
func built() fstest.MapFS {
	return fstest.MapFS{
		"index.html":            {Data: []byte("<!doctype html><div id=root></div>")},
		"assets/index-a1b2.js":  {Data: []byte("console.log(1)")},
		"assets/index-a1b2.css": {Data: []byte("body{}")},
		"embed-placeholder.txt": {Data: []byte("placeholder")},
	}
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHandlerServesFiles(t *testing.T) {
	h := handler(built())
	cases := []struct {
		path, body, contentType string
	}{
		{"/", "<div id=root>", "text/html"},
		{"/assets/index-a1b2.js", "console.log(1)", "text/javascript"},
		{"/assets/index-a1b2.css", "body{}", "text/css"},
	}
	for _, c := range cases {
		rec := get(t, h, http.MethodGet, c.path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s: %d %q", c.path, rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, c.contentType) {
			t.Errorf("%s: Content-Type %q, voglio %s", c.path, ct, c.contentType)
		}
	}
}

// Le route di react-router, dopo un reload, devono dare index.html.
func TestHandlerFallsBackToIndex(t *testing.T) {
	h := handler(built())
	// Un device_id può avere un punto: non è un file mancante.
	for _, path := range []string{"/devices", "/devices/pi_a", "/devices/raspberrypi.local", "/devices/pi-1.2", "/assets", "/assets/"} {
		rec := get(t, h, http.MethodGet, path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<div id=root>") {
			t.Errorf("%s: %d %q, voglio index.html", path, rec.Code, rec.Body)
		}
	}
}

// Un file sotto assets/ che non esiste (un hash vecchio) è un 404, non
// index.html: il browser riceverebbe HTML al posto di uno script.
func TestHandlerMissingAssetIs404(t *testing.T) {
	h := handler(built())
	for _, path := range []string{"/assets/old-9999.js", "/assets/old-9999.css"} {
		if rec := get(t, h, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, voglio 404", path, rec.Code)
		}
	}
}

func TestHandlerMethods(t *testing.T) {
	h := handler(built())
	if rec := get(t, h, http.MethodHead, "/"); rec.Code != http.StatusOK {
		t.Errorf("HEAD /: %d", rec.Code)
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := get(t, h, m, "/devices")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, voglio 405", m, rec.Code)
		}
		if a := rec.Header().Get("Allow"); a != "GET, HEAD" {
			t.Errorf("%s: Allow %q", m, a)
		}
	}
}

func TestHandlerNotBuilt(t *testing.T) {
	h := handler(fstest.MapFS{"embed-placeholder.txt": {Data: []byte("placeholder")}})
	for _, path := range []string{"/", "/devices/pi_a", "/embed-placeholder.txt"} {
		rec := get(t, h, http.MethodGet, path)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "npm run build") {
			t.Errorf("%s: %d %q, voglio la pagina notBuilt", path, rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", path, ct)
		}
	}
}

// Il segnaposto deve essere nel binario: senza, go:embed non compila e chi
// non ha Node non può più compilare il server.
func TestEmbedHasPlaceholder(t *testing.T) {
	if _, err := fs.Stat(dist, "dist/embed-placeholder.txt"); err != nil {
		t.Fatal(err)
	}
}
