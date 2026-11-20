package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// restartServer: un server con la config e il riavvio abilitati; restarts
// conta le chiamate alla funzione di riavvio (che qui non spegne nulla).
func restartServer(t *testing.T, check ConfigCheck) (http.Handler, string, *int) {
	t.Helper()
	s, _, _ := testServer(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(configV1), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfigFile(path, check); err != nil {
		t.Fatal(err)
	}
	a, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	s.SetAudit(a)
	restarts := 0
	s.SetRestart(func() { restarts++ })
	return s.AdminRoutes(testAdminToken), path, &restarts
}

func TestRestart(t *testing.T) {
	var seen [2][]byte
	h, path, restarts := restartServer(t, func(current, next []byte) error {
		seen = [2][]byte{current, next}
		return nil
	})
	rec := adminReq(h, http.MethodPost, "/api/v1/admin/restart", "", bearer(testAdminToken))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"restarting":true`) {
		t.Fatalf("restart: %d %s", rec.Code, rec.Body)
	}
	if *restarts != 1 {
		t.Errorf("funzione di riavvio chiamata %d volte, voglio 1", *restarts)
	}
	// Il controllo sul file così com'è: current e next uguali.
	if string(seen[0]) != configV1 || string(seen[1]) != configV1 {
		t.Errorf("controllo su %q, %q", seen[0], seen[1])
	}
	lines := readAudit(t, filepath.Dir(path))
	if len(lines) != 1 {
		t.Fatalf("registro: %v", lines)
	}
	cfg, _ := lines[0]["config"].(map[string]any)
	if lines[0]["action"] != "restart" || lines[0]["who"] != actorToken || cfg["path"] != path || cfg["hash"] != hashOf([]byte(configV1)) {
		t.Errorf("riga: %v", lines[0])
	}
	if _, ok := cfg["backup"]; ok {
		t.Errorf("un riavvio non ha backup: %v", cfg)
	}
}

// Un config.json che non partirebbe: niente riavvio, il server resta su.
func TestRestartRefusesABrokenConfig(t *testing.T) {
	h, path, restarts := restartServer(t, func(_, _ []byte) error {
		return errors.New("stream config: stream.max_inflight must be >= 1, got 0")
	})
	rec := adminReq(h, http.MethodPost, "/api/v1/admin/restart", "", bearer(testAdminToken))
	body := rec.Body.String()
	if rec.Code != http.StatusBadRequest || !strings.Contains(body, `"invalid_config"`) || !strings.Contains(body, "would not start the server: stream config:") {
		t.Fatalf("restart con config rotta: %d %s", rec.Code, body)
	}
	if *restarts != 0 || len(readAudit(t, filepath.Dir(path))) != 0 {
		t.Errorf("riavvii %d, righe %v: voglio nessuno", *restarts, readAudit(t, filepath.Dir(path)))
	}
}

func TestRestartUnreadableConfig(t *testing.T) {
	h, path, restarts := restartServer(t, func(_, _ []byte) error { return nil })
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if rec := adminReq(h, http.MethodPost, "/api/v1/admin/restart", "", bearer(testAdminToken)); rec.Code != http.StatusInternalServerError {
		t.Fatalf("config sparita: %d %s", rec.Code, rec.Body)
	}
	if *restarts != 0 {
		t.Error("niente riavvio senza config")
	}
}

func TestRestartDisabled(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.AdminRoutes(testAdminToken)
	rec := adminReq(h, http.MethodPost, "/api/v1/admin/restart", "", bearer(testAdminToken))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
		t.Fatalf("senza SetRestart: %d %s", rec.Code, rec.Body)
	}

	// Con il riavvio ma senza config: nessun controllo possibile, niente riavvio.
	s, _, _ = testServer(t)
	called := false
	s.SetRestart(func() { called = true })
	h = s.AdminRoutes(testAdminToken)
	if rec := adminReq(h, http.MethodPost, "/api/v1/admin/restart", "", bearer(testAdminToken)); rec.Code != http.StatusNotFound || called {
		t.Fatalf("senza config: %d, chiamato %v", rec.Code, called)
	}
}

func TestRestartNeedsLogin(t *testing.T) {
	h, _, restarts := restartServer(t, func(_, _ []byte) error { return nil })
	if rec := adminReq(h, http.MethodPost, "/api/v1/admin/restart", ""); rec.Code != http.StatusUnauthorized || *restarts != 0 {
		t.Fatalf("senza credenziali: %d, riavvii %d", rec.Code, *restarts)
	}
	// Un altro sito non può far riavviare il server dal browser di chi è loggato.
	rec := adminReq(h, http.MethodPost, "/api/v1/admin/restart", "", bearer(testAdminToken), func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if rec.Code != http.StatusForbidden || *restarts != 0 {
		t.Fatalf("cross-site: %d, riavvii %d", rec.Code, *restarts)
	}
}
