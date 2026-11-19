// Package dashboard serve la dashboard web compilata (sorgenti in
// dashboard/, build con npm run build), inclusa nel binario: il server non
// ha bisogno di Node né di file accanto (docs/DASHBOARD.md).
package dashboard

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist è l'output di Vite (build.outDir in dashboard/vite.config.ts).
// all: include anche i file che iniziano con . o _. In git c'è solo
// embed-placeholder.txt: senza nessun file go:embed non compila, e chi non ha
// Node deve poter compilare il server lo stesso.
//
//go:embed all:dist
var dist embed.FS

// notBuilt è la pagina mostrata quando il binario è stato compilato senza
// build della dashboard (in dist c'è solo il segnaposto).
const notBuilt = `<!doctype html>
<meta charset="utf-8">
<title>SCIoT dashboard not built</title>
<p>The dashboard is not in this binary. Build it, then rebuild the server:</p>
<pre>cd dashboard && npm ci && npm run build
go build ./cmd/server</pre>
<p>The admin API works anyway (docs/ADMIN_API.md).</p>
`

// Handler serve la dashboard. Le route /api/ non passano di qui: le
// gestisce server.AdminRoutes.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // impossibile: "dist" è il pattern di go:embed
	}
	return handler(sub)
}

// handler serve i file di files (la radice è il contenuto di dist). È
// separato da Handler per i test, che passano un fstest.MapFS.
//
// Regole:
//   - senza index.html in files: notBuilt, 404, per ogni richiesta;
//   - solo GET e HEAD; altri metodi: 405 con l'header Allow;
//   - un file che esiste: il file (http.FileServerFS);
//   - un file mancante sotto assets/ ("/assets/x.js", dove Vite mette i file
//     con l'hash): 404. Con index.html il browser riceverebbe HTML al posto
//     di uno script;
//   - ogni altro percorso che non è un file ("/devices/pi_a", anche
//     "/assets", che è una cartella): index.html. È una route di
//     react-router: dopo un reload il browser chiede quel percorso al
//     server, e la pagina giusta la sceglie il router nel browser. Non si
//     guarda l'estensione: un device_id può avere un punto
//     ("/devices/raspberrypi.local").
func handler(files fs.FS) http.Handler {
	if _, err := fs.Stat(files, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, notBuilt)
		})
	}

	fileServer := http.FileServerFS(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// La radice "/" diventa "", che per fs.Stat non è un nome valido:
		// la serve FileServerFS (è index.html).
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			fileServer.ServeHTTP(w, r)
			return
		}
		if info, err := fs.Stat(files, name); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}

		// Non è un file. Sotto assets/ è un asset mancante: 404, non HTML.
		if strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}
		// Altrimenti è una route di react-router, anche con un punto nel nome.
		http.ServeFileFS(w, r, files, "index.html")
	})
}
