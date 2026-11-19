package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	configV1 = `{"server": {}, "models": []}` + "\n"
	configV2 = `{"server": {"heartbeat_interval_ms": 500}, "models": []}` + "\n"
)

// fakeCheck: rifiuta un testo con "bad"; ricorda il current che ha visto.
type fakeCheck struct{ current []byte }

func (f *fakeCheck) check(current, next []byte) error {
	f.current = current
	if strings.Contains(string(next), "bad") {
		return errors.New("server config: bad value")
	}
	return nil
}

// configServer: un Server con la config in una cartella temporanea, il
// registro delle modifiche nella stessa cartella, e la porta admin.
func configServer(t *testing.T) (*Server, http.Handler, string, *fakeCheck) {
	t.Helper()
	s, _, _ := testServer(t)
	s.cfg.MaxBodyBytes = 64 << 10
	// EvalSymlinks: su macOS la cartella temporanea /var/… è un link a
	// /private/var/…, e SetConfigFile risolve i link.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(configV1), 0o640); err != nil {
		t.Fatal(err)
	}
	fc := &fakeCheck{}
	if err := s.SetConfigFile(path, fc.check); err != nil {
		t.Fatal(err)
	}
	a, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	s.SetAudit(a)
	return s, s.AdminRoutes(testAdminToken), path, fc
}

func configBody(t *testing.T, content, baseHash string) string {
	t.Helper()
	b, err := json.Marshal(adminConfigRequest{Content: content, BaseHash: baseHash})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func getConfig(t *testing.T, h http.Handler) adminConfigResponse {
	t.Helper()
	rec := adminReq(h, http.MethodGet, "/api/v1/admin/config", "", bearer(testAdminToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config: %d %s", rec.Code, rec.Body)
	}
	var resp adminConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func putConfig(t *testing.T, h http.Handler, content, baseHash string) (int, map[string]any) {
	t.Helper()
	rec := adminReq(h, http.MethodPut, "/api/v1/admin/config", configBody(t, content, baseHash), bearer(testAdminToken))
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("PUT: corpo non JSON (%d)", rec.Code)
	}
	return rec.Code, m
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// backups: i file .bak della cartella.
func backups(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "config.json.*.bak"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestConfigDisabled(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		rec := adminReq(h, m, "/api/v1/admin/config", configBody(t, configV1, "x"), bearer(testAdminToken))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("%s senza SetConfigFile: %d %s", m, rec.Code, rec.Body)
		}
	}
}

func TestConfigNeedsLogin(t *testing.T) {
	_, h, _, _ := configServer(t)
	if rec := adminReq(h, http.MethodGet, "/api/v1/admin/config", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("senza credenziali: %d", rec.Code)
	}
}

func TestConfigGet(t *testing.T) {
	_, h, path, _ := configServer(t)
	c := getConfig(t, h)
	if c.Path != path || c.Content != configV1 || c.Hash != hashOf([]byte(configV1)) || c.ModifiedSinceStart {
		t.Fatalf("GET: %+v", c)
	}
	// Cambiato a mano dopo l'avvio: il server gira con un'altra versione.
	if err := os.WriteFile(path, []byte(configV2), 0o640); err != nil {
		t.Fatal(err)
	}
	if c := getConfig(t, h); c.Content != configV2 || !c.ModifiedSinceStart {
		t.Errorf("dopo la modifica a mano: %+v", c)
	}
}

func TestConfigValidate(t *testing.T) {
	_, h, _, fc := configServer(t)
	rec := adminReq(h, http.MethodPost, "/api/v1/admin/config/validate", configBody(t, configV2, ""), bearer(testAdminToken))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"valid":true`) {
		t.Fatalf("valida: %d %s", rec.Code, rec.Body)
	}
	if string(fc.current) != configV1 {
		t.Errorf("il controllo deve ricevere il file in uso, ha ricevuto %q", fc.current)
	}
	rec = adminReq(h, http.MethodPost, "/api/v1/admin/config/validate", configBody(t, `{"bad": 1}`, ""), bearer(testAdminToken))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_config"`) || !strings.Contains(rec.Body.String(), "server config: bad value") {
		t.Fatalf("non valida: %d %s", rec.Code, rec.Body)
	}
}

func TestConfigSave(t *testing.T) {
	s, h, path, _ := configServer(t)
	s.now = func() time.Time { return time.Date(2026, 11, 19, 15, 4, 5, 0, time.Local) }
	dir := filepath.Dir(path)

	code, m := putConfig(t, h, configV2, hashOf([]byte(configV1)))
	if code != http.StatusOK {
		t.Fatalf("PUT: %d %v", code, m)
	}
	if m["changed"] != true || m["hash"] != hashOf([]byte(configV2)) || m["restart_required"] != true || m["backup"] != "config.json.20261119-150405.bak" {
		t.Errorf("risposta: %v", m)
	}
	if got := readFile(t, path); got != configV2 {
		t.Errorf("file: %q", got)
	}
	// Il backup ha il contenuto di prima, permessi 600.
	bak := filepath.Join(dir, "config.json.20261119-150405.bak")
	if got := readFile(t, bak); got != configV1 {
		t.Errorf("backup: %q", got)
	}
	if info, _ := os.Stat(bak); info.Mode().Perm() != 0o600 {
		t.Errorf("permessi del backup %o", info.Mode().Perm())
	}
	// La scrittura tiene i permessi del file e non lascia temporanei.
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("permessi del file %o, voglio 640", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("file temporaneo rimasto: %s", e.Name())
		}
	}
	// Il registro: chi, quale file, quale backup, quale hash.
	lines := readAudit(t, dir)
	if len(lines) != 1 {
		t.Fatalf("registro: %v", lines)
	}
	cfg, _ := lines[0]["config"].(map[string]any)
	if lines[0]["action"] != "config" || lines[0]["who"] != actorToken || cfg["path"] != path || cfg["backup"] != "config.json.20261119-150405.bak" || cfg["hash"] != hashOf([]byte(configV2)) {
		t.Errorf("riga del registro: %v", lines[0])
	}
	if _, ok := lines[0]["device_id"]; ok {
		t.Errorf("riga della config con campi del device: %v", lines[0])
	}

	// Il GET dopo: contenuto nuovo, diverso dall'avvio.
	if c := getConfig(t, h); c.Content != configV2 || !c.ModifiedSinceStart {
		t.Errorf("GET dopo il PUT: %+v", c)
	}
}

func TestConfigSaveConflict(t *testing.T) {
	_, h, path, _ := configServer(t)
	stale := hashOf([]byte(configV1))
	// Qualcuno cambia il file dopo il GET.
	if err := os.WriteFile(path, []byte(configV2), 0o640); err != nil {
		t.Fatal(err)
	}
	code, m := putConfig(t, h, `{"server": {}, "models": [], "x": 1}`, stale)
	if code != http.StatusConflict || errCode(m) != "config_changed" {
		t.Fatalf("PUT su una versione vecchia: %d %v", code, m)
	}
	if readFile(t, path) != configV2 || len(backups(t, filepath.Dir(path))) != 0 {
		t.Error("dopo il 409 il file deve restare quello e nessun backup")
	}
}

func TestConfigSaveInvalid(t *testing.T) {
	_, h, path, _ := configServer(t)
	code, m := putConfig(t, h, `{"bad": 1}`, hashOf([]byte(configV1)))
	if code != http.StatusBadRequest || errCode(m) != "invalid_config" {
		t.Fatalf("PUT non valido: %d %v", code, m)
	}
	if readFile(t, path) != configV1 || len(backups(t, filepath.Dir(path))) != 0 {
		t.Error("dopo il 400 il file deve restare quello e nessun backup")
	}
	if code, m := putConfig(t, h, configV2, ""); code != http.StatusBadRequest || errCode(m) != "invalid_request" {
		t.Errorf("senza base_hash: %d %v", code, m)
	}
}

func TestConfigSaveUnchanged(t *testing.T) {
	_, h, path, _ := configServer(t)
	code, m := putConfig(t, h, configV1, hashOf([]byte(configV1)))
	if code != http.StatusOK || m["changed"] != false || m["restart_required"] != false {
		t.Fatalf("PUT dello stesso contenuto: %d %v", code, m)
	}
	if len(backups(t, filepath.Dir(path))) != 0 || len(readAudit(t, filepath.Dir(path))) != 0 {
		t.Error("nessun backup e nessuna riga se non cambia nulla")
	}
}

// config.json è un link (per esempio verso ~/dotfiles): si scrive il file
// vero, il link resta un link.
func TestConfigSaveThroughSymlink(t *testing.T) {
	s, _, _ := testServer(t)
	s.cfg.MaxBodyBytes = 64 << 10
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte(configV1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfigFile(link, (&fakeCheck{}).check); err != nil {
		t.Fatal(err)
	}
	h := s.AdminRoutes(testAdminToken)
	if code, m := putConfig(t, h, configV2, hashOf([]byte(configV1))); code != http.StatusOK {
		t.Fatalf("PUT: %d %v", code, m)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("il link non è più un link: %v %v", info, err)
	}
	if readFile(t, target) != configV2 {
		t.Error("il file vero non è stato scritto")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "real.json.*.bak")); len(m) != 1 {
		t.Errorf("backup accanto al file vero: %v", m)
	}
}

func TestBackupConfigNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	now := time.Date(2026, 11, 19, 15, 4, 5, 0, time.Local)
	name, err := backupConfig(path, []byte("a"), now)
	if err != nil || name != "config.json.20261119-150405.bak" {
		t.Fatalf("%q %v", name, err)
	}
	if _, err := backupConfig(path, []byte("b"), now); err == nil {
		t.Fatal("un secondo backup nello stesso secondo non deve sovrascrivere il primo")
	}
	if readFile(t, filepath.Join(dir, name)) != "a" {
		t.Error("il primo backup è stato sovrascritto")
	}
	if !regexp.MustCompile(`^config\.json\.\d{8}-\d{6}\.bak$`).MatchString(name) {
		t.Errorf("nome %q", name)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if readFile(t, path) != "new" {
		t.Error("contenuto")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("permessi %o", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("file rimasti: %v", entries)
	}
	// Un file che non esiste: errore, niente temporanei.
	if err := writeFileAtomic(filepath.Join(dir, "missing.json"), []byte("x")); err == nil {
		t.Error("file inesistente: voglio un errore")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temporaneo rimasto dopo l'errore: %v", entries)
	}
}
