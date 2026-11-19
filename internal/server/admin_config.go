package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// config.json dalla dashboard (docs/ADMIN_API.md, config).
//
//	GET  /api/v1/admin/config           il file, il suo hash, se è cambiato dall'avvio
//	POST /api/v1/admin/config/validate  partirebbe? (gli stessi controlli dell'avvio)
//	PUT  /api/v1/admin/config           valida, backup, scrittura atomica, registro
//
// Il server in esecuzione non cambia: la config nuova vale dal prossimo
// avvio. Il controllo (configCheck) lo dà main: sono i controlli
// dell'avvio (cmd/server/config.go), che il server non può importare
// (internal/config importa già internal/server).

// Codici di errore della config.
const (
	errCodeInvalidConfig = "invalid_config" // 400: non partirebbe, o cambia worker.command
	errCodeConfigChanged = "config_changed" // 409: il file è cambiato dopo il GET
)

// ConfigCheck dice se next partirebbe, dato current, il file in uso.
type ConfigCheck func(current, next []byte) error

// configFile è lo stato della config per gli handler.
type configFile struct {
	path       string      // il file vero (link simbolici risolti)
	check      ConfigCheck // nil = modifica dalla dashboard spenta
	loadedHash string      // hash del contenuto all'avvio

	mu sync.Mutex // un PUT alla volta: leggi, confronta, scrivi
}

// SetConfigFile abilita la config dalla dashboard: path è il file dato con
// -config, check i controlli dell'avvio. Va chiamata prima di AdminRoutes.
//
// Un link simbolico (config.json → ~/dotfiles/…) si risolve qui: la
// scrittura atomica (rename) sostituirebbe il link con un file, e il file
// vero resterebbe vecchio.
func (s *Server) SetConfigFile(path string, check ConfigCheck) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(real)
	if err != nil {
		return err
	}
	s.config = &configFile{path: real, check: check, loadedHash: hashOf(data)}
	return nil
}

// hashOf è lo sha256 di data in esadecimale: il "base_hash" che il client
// rimanda nel PUT per dire su quale versione ha lavorato.
func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// adminConfigResponse è la risposta di GET /api/v1/admin/config.
type adminConfigResponse struct {
	Path               string `json:"path"`
	Content            string `json:"content"`
	Hash               string `json:"hash"`
	ModifiedSinceStart bool   `json:"modified_since_start"` // il server gira con un'altra versione
}

// adminConfigRequest è il corpo di validate e PUT.
type adminConfigRequest struct {
	Content  string `json:"content"`
	BaseHash string `json:"base_hash,omitempty"` // solo PUT: l'hash del GET su cui si è lavorato
}

// adminConfigSaveResponse è la risposta 200 del PUT.
type adminConfigSaveResponse struct {
	Changed bool   `json:"changed"` // false: il contenuto era già quello, nulla scritto
	Hash    string `json:"hash"`
	Backup  string `json:"backup,omitempty"` // nome del backup, nella cartella del file
	// RestartRequired: il server gira ancora con la config dell'avvio.
	RestartRequired bool `json:"restart_required"`
}

// configEnabled risponde 404 se la config dalla dashboard è spenta.
func (s *Server) configEnabled(w http.ResponseWriter) bool {
	if s.config == nil || s.config.check == nil {
		writeError(w, http.StatusNotFound, errCodeNotFound, "config editing is not enabled")
		return false
	}
	return true
}

// handleAdminConfig: GET /api/v1/admin/config.
func (s *Server) handleAdminConfig(w http.ResponseWriter, _ *http.Request) {
	if !s.configEnabled(w) {
		return
	}
	data, err := os.ReadFile(s.config.path)
	if err != nil {
		log.Printf("admin config: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot read the config file")
		return
	}
	h := hashOf(data)
	writeJSON(w, http.StatusOK, adminConfigResponse{
		Path:               s.config.path,
		Content:            string(data),
		Hash:               h,
		ModifiedSinceStart: h != s.config.loadedHash,
	})
}

// handleAdminConfigValidate: POST /api/v1/admin/config/validate.
//
//	200 {"valid": true}
//	400 invalid_config → il motivo, lo stesso messaggio dell'avvio
func (s *Server) handleAdminConfigValidate(w http.ResponseWriter, r *http.Request) {
	if !s.configEnabled(w) {
		return
	}
	var req adminConfigRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	current, err := os.ReadFile(s.config.path)
	if err != nil {
		current = nil // file sparito o illeggibile: per il controllo vale "nessun comando"
	}
	if err := s.config.check(current, []byte(req.Content)); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidConfig, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

// handleAdminConfigSave: PUT /api/v1/admin/config.
//
//	200 → adminConfigSaveResponse
//	400 invalid_request → JSON malformato, base_hash mancante
//	400 invalid_config  → non partirebbe, o cambia worker.command
//	409 config_changed  → il file non ha più base_hash: qualcuno l'ha cambiato
//	500                 → backup o scrittura falliti (il file resta quello di prima)
func (s *Server) handleAdminConfigSave(w http.ResponseWriter, r *http.Request) {
	if !s.configEnabled(w) {
		return
	}
	var req adminConfigRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if req.BaseHash == "" {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "base_hash is required: send the hash of the config you edited")
		return
	}
	next := []byte(req.Content)

	c := s.config
	c.mu.Lock()
	defer c.mu.Unlock()

	current, err := os.ReadFile(c.path)
	if err != nil {
		log.Printf("admin config: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot read the config file")
		return
	}
	// Sotto c.mu: nessun altro PUT cambia il file tra questo confronto e la
	// scrittura. Una modifica a mano nel frattempo la cattura l'hash.
	if hashOf(current) != req.BaseHash {
		writeError(w, http.StatusConflict, errCodeConfigChanged,
			"the config file changed since you loaded it: reload and apply your changes again")
		return
	}
	if err := c.check(current, next); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidConfig, err.Error())
		return
	}
	// Stesso contenuto: niente backup e niente riga nel registro.
	if bytes.Equal(current, next) {
		writeJSON(w, http.StatusOK, adminConfigSaveResponse{
			Changed:         false,
			Hash:            req.BaseHash,
			RestartRequired: hashOf(current) != c.loadedHash,
		})
		return
	}
	// Il backup prima della scrittura: se fallisce, il file non è toccato.
	backup, err := backupConfig(c.path, current, s.now())
	if err != nil {
		log.Printf("admin config: backup: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot back up the config file: it was not changed")
		return
	}
	if err := writeFileAtomic(c.path, next); err != nil {
		log.Printf("admin config: write: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot write the config file: it was not changed")
		return
	}
	h := hashOf(next)
	s.auditConfigSave(r, auditConfigEntry{Path: c.path, Backup: backup, Hash: h})
	writeJSON(w, http.StatusOK, adminConfigSaveResponse{
		Changed:         true,
		Hash:            h,
		Backup:          backup,
		RestartRequired: h != c.loadedHash,
	})
}

// backupConfig scrive data in <path>.<YYYYMMDD-HHMMSS>.bak (ora locale,
// come i file di registrazione) accanto a path e restituisce il nome del
// backup (solo il nome, senza cartella).
//
// Un backup con lo stesso nome (due salvataggi nello stesso secondo) non si
// sovrascrive: O_EXCL fallisce, e il salvataggio con lui. shortcut: due
// salvataggi nello stesso secondo danno 500 al secondo; se succede davvero,
// aggiungere i millisecondi al nome.
func backupConfig(path string, data []byte, now time.Time) (string, error) {
	name := filepath.Base(path) + "." + now.Format("20060102-150405") + ".bak"
	full := filepath.Join(filepath.Dir(path), name)
	// 0o600: la config può contenere percorsi e nomi della macchina.
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(full) // niente backup a metà
		return "", err
	}
	// L'errore di Close conta: è lì che si scopre un disco pieno.
	if err := f.Close(); err != nil {
		os.Remove(full)
		return "", err
	}
	return name, nil
}

// writeFileAtomic sostituisce il contenuto di path con data in modo che chi
// legge path veda il file vecchio o quello nuovo, mai uno a metà, anche se
// il server muore durante la scrittura.
//
// Come: un file temporaneo nella stessa cartella (rename è atomico solo
// dentro lo stesso filesystem), Sync (i dati sul disco prima del rename),
// stessi permessi del file vecchio, poi os.Rename sopra path.
func writeFileAtomic(path string, data []byte) error {
	info, err := os.Stat(path) // i permessi da tenere
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	// Dopo il rename il temporaneo non esiste più: la pulizia serve solo se
	// qualcosa fallisce prima.
	ok := false
	defer func() {
		if !ok {
			tmp.Close() // già chiuso su alcuni percorsi: l'errore non interessa
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil { // i dati sul disco prima del rename
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	return nil
}
