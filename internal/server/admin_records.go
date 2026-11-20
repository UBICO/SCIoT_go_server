package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// Registrazioni e registro delle modifiche, in sola lettura
// (docs/ADMIN_API.md):
//
//	GET /api/v1/admin/recordings          i file di record.dir, dal più recente
//	GET /api/v1/admin/recordings/{name}   il download di un file
//	GET /api/v1/admin/changelog           le ultime righe di sciot-audit.jsonl
//
// Niente cancellazione: le registrazioni sono i dati degli esperimenti, si
// cancellano a mano sulla macchina.

// recordingName è il nome di un file di registrazione (record.Open):
// sciot-YYYYMMDD-HHMMSS.jsonl. Solo questi si elencano e si scaricano: un
// nome come "../config.json" non corrisponde, quindi il download non esce
// mai da record.dir.
var recordingName = regexp.MustCompile(`^sciot-\d{8}-\d{6}\.jsonl$`)

// Limiti del registro delle modifiche.
const (
	changelogDefault = 200
	changelogMax     = 1000
)

// SetRecordDir dice al server la cartella della registrazione (record.dir),
// per elencarla e per leggere il registro delle modifiche. Senza: la
// registrazione è spenta, e le due route lo dicono.
func (s *Server) SetRecordDir(dir string) { s.recordDir = dir }

// adminRecording è un file di registrazione.
type adminRecording struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	Modified  time.Time `json:"modified"`
	// Current: il file di questo avvio, ancora in scrittura. Scaricato
	// adesso, l'ultima riga può essere a metà (il recorder scrive con un
	// buffer).
	Current bool `json:"current"`
}

type adminRecordingsResponse struct {
	Recording bool             `json:"recording"` // false: nessuna sezione "record"
	Dir       string           `json:"dir,omitempty"`
	Files     []adminRecording `json:"files"` // dal più recente; [] e non null
}

// handleAdminRecordings: GET /api/v1/admin/recordings.
func (s *Server) handleAdminRecordings(w http.ResponseWriter, _ *http.Request) {
	files := []adminRecording{} // [] e non null
	if s.recordDir == "" {
		writeJSON(w, http.StatusOK, adminRecordingsResponse{Recording: false, Files: files})
		return
	}
	entries, err := os.ReadDir(s.recordDir)
	if err != nil {
		log.Printf("admin recordings: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot read the recording directory")
		return
	}
	// Stats su un Recorder nil dà File "": nessun file corrente.
	current := filepath.Base(s.rec.Stats().File)
	for _, e := range entries {
		if e.IsDir() || !recordingName.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // sparito nel frattempo
		}
		files = append(files, adminRecording{
			Name:      e.Name(),
			SizeBytes: info.Size(),
			Modified:  info.ModTime(),
			Current:   e.Name() == current,
		})
	}
	// Data e ora nel nome hanno le cifre in ordine: il nome al contrario è
	// l'ordine dal più recente.
	slices.SortFunc(files, func(a, b adminRecording) int { return strings.Compare(b.Name, a.Name) })
	writeJSON(w, http.StatusOK, adminRecordingsResponse{Recording: true, Dir: s.recordDir, Files: files})
}

// handleAdminRecordingDownload: GET /api/v1/admin/recordings/{name}.
//
//	200 il file (application/x-ndjson, come allegato); Range supportato
//	404 not_found → registrazione spenta, nome non valido, file inesistente
func (s *Server) handleAdminRecordingDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// La regexp è la difesa: niente "/" né "..".
	if s.recordDir == "" || !recordingName.MatchString(name) {
		writeError(w, http.StatusNotFound, errCodeNotFound, "no such recording")
		return
	}
	f, err := os.Open(filepath.Join(s.recordDir, name))
	if err != nil {
		writeError(w, http.StatusNotFound, errCodeNotFound, "no such recording")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, errCodeNotFound, "no such recording")
		return
	}
	n, err := completeLines(f, info.Size())
	if err != nil {
		log.Printf("admin recordings: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot read the recording")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	// Il nome è già validato: niente virgolette da proteggere.
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// Gestisce HEAD, Range e If-Modified-Since, sui soli primi n byte.
	http.ServeContent(w, r, name, info.ModTime(), io.NewSectionReader(f, 0, n))
}

// tailScan: quanti byte dalla fine leggere per cercare l'ultimo a capo. Il
// recorder scrive righe intere con un buffer, quindi la parte a metà è al più
// grande quanto il buffer, molto meno di così.
const tailScan = 64 << 10

// completeLines restituisce la lunghezza di f fino all'ultimo '\n' compreso:
// il file di questo avvio, ancora in scrittura, può finire con una riga a
// metà, e sciot-replay si fermerebbe su quella (internal/replay: "line N:
// unexpected end of JSON input"). Un file completo finisce con '\n': n == size.
//
// Si leggono solo gli ultimi tailScan byte. Senza '\n' lì dentro (una riga
// più lunga di 64 KiB a metà, o un file senza a capo) si restituisce size:
// meglio il file intero che uno vuoto.
func completeLines(f io.ReaderAt, size int64) (int64, error) {
	start := max(0, size-tailScan)
	buf := make([]byte, size-start)
	// io.EOF con il buffer pieno non è un errore (ReaderAt può darlo sull'ultimo byte).
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	i := bytes.LastIndexByte(buf, '\n')
	if i < 0 {
		return size, nil
	}
	return start + int64(i) + 1, nil
}

// adminChangelogResponse: le righe del registro, dalla più recente, così come
// sono nel file (json.RawMessage: la dashboard legge i campi di ADMIN_API.md).
type adminChangelogResponse struct {
	Enabled bool              `json:"enabled"` // false: nessuna sezione "record"
	Entries []json.RawMessage `json:"entries"`
}

// handleAdminChangelog: GET /api/v1/admin/changelog?limit=N (default 200,
// massimo 1000).
//
//	200 → adminChangelogResponse
//	400 invalid_request → limit non è un intero in [1, 1000]
func (s *Server) handleAdminChangelog(w http.ResponseWriter, r *http.Request) {
	limit := changelogDefault
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > changelogMax {
			writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "limit must be an integer in [1, 1000]")
			return
		}
		limit = n
	}
	entries := []json.RawMessage{} // [] e non null
	if s.recordDir == "" {
		writeJSON(w, http.StatusOK, adminChangelogResponse{Enabled: false, Entries: entries})
		return
	}
	// shortcut: legge tutto il file; il registro cresce di una riga per azione
	// admin (poche al giorno). Leggerlo dalla fine solo se arriva a decine di MB.
	data, err := os.ReadFile(filepath.Join(s.recordDir, AuditFile))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("admin changelog: %v", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "cannot read the change log")
			return
		}
		data = nil
	}
	// Dalla più recente; una riga vuota o a metà (crash) non rompe la pagina.
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0 && len(entries) < limit; i-- {
		if len(lines[i]) > 0 && json.Valid(lines[i]) {
			entries = append(entries, json.RawMessage(lines[i]))
		}
	}
	writeJSON(w, http.StatusOK, adminChangelogResponse{Enabled: true, Entries: entries})
}
