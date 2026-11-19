package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// Autenticazione della porta admin (docs/ADMIN_API.md).
//
// Due modi, lo stesso token:
//   - Authorization: Bearer <token>, per curl e script;
//   - il cookie di POST /api/v1/admin/login, per il browser: EventSource
//     (SSE) non può aggiungere header, un cookie lo manda da solo.
//
// Il cookie contiene un id di sessione casuale, non il token: rubarlo dà
// accesso per al massimo adminSessionTTL, e il token non esce mai dal login.

const (
	// adminCookie è il nome del cookie di sessione.
	adminCookie = "sciot_admin"
	// adminSessionTTL: dopo 12 ore senza riavvio si rifà il login.
	adminSessionTTL = 12 * time.Hour
	// MinAdminTokenLen: un token più corto si indovina troppo in fretta.
	MinAdminTokenLen = 16

	// Codici di errore della porta admin: non fanno parte del protocollo dei
	// device, quindi restano qui e non in internal/api.
	errCodeUnauthorized = "unauthorized"
	errCodeNotFound     = "not_found"
)

// adminSession è una sessione del browser.
type adminSession struct {
	name    string // chi ha fatto il login: per il registro delle modifiche
	expires time.Time
}

// adminAuth controlla le richieste della porta admin.
type adminAuth struct {
	// token vuoto = nessuna autenticazione. main lo permette solo su
	// un indirizzo di loopback (CheckAdminAddr).
	token []byte
	now   func() time.Time

	// mu protegge sessions.
	mu sync.Mutex
	// shortcut: le sessioni scadute si cancellano solo quando qualcuno le
	// usa; la map cresce di una voce per login riuscito, cioè per chi ha il
	// token. Servirà una pulizia periodica solo se i login diventano migliaia.
	sessions map[string]adminSession
}

func newAdminAuth(token string, now func() time.Time) *adminAuth {
	return &adminAuth{token: []byte(token), now: now, sessions: make(map[string]adminSession)}
}

// tokenOK confronta t con il token in tempo costante: con == il tempo di
// risposta dipenderebbe da quanti caratteri iniziali sono giusti, e il
// token si potrebbe indovinare un carattere alla volta.
func (a *adminAuth) tokenOK(t string) bool {
	if len(a.token) == 0 {
		return true
	}
	// ConstantTimeCompare esce subito se le lunghezze differiscono: rivela
	// solo la lunghezza, che non è un segreto.
	return subtle.ConstantTimeCompare([]byte(t), a.token) == 1
}

// newSession crea una sessione per name e ne restituisce l'id.
func (a *adminAuth) newSession(name string) (string, error) {
	// crypto/rand, non math/rand: l'id deve essere imprevedibile.
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(b) // sicuro in un cookie

	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[id] = adminSession{name: name, expires: a.now().Add(adminSessionTTL)}
	return id, nil
}

// session restituisce la sessione id se esiste e non è scaduta. Una
// sessione scaduta viene cancellata.
func (a *adminAuth) session(id string) (adminSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok {
		return adminSession{}, false
	}
	if !a.now().Before(s.expires) {
		delete(a.sessions, id)
		return adminSession{}, false
	}
	return s, true
}

// wrap fa passare next solo alle richieste autenticate: Bearer valido o
// cookie di una sessione valida. Le altre ricevono 401. A next passa anche
// chi è (withActor), per il registro delle modifiche (audit.go):
//   - cookie di una sessione valida → il nome del login;
//   - Bearer valido → actorToken;
//   - senza token configurato e senza sessione → actorAnonymous.
func (a *adminAuth) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(a.token) == 0 {
			// Anche senza token il login funziona e dà un nome.
			who := actorAnonymous
			if c, err := r.Cookie(adminCookie); err == nil {
				if sess, ok := a.session(c.Value); ok {
					who = sess.name
				}
			}
			next.ServeHTTP(w, withActor(r, who))
			return
		}
		// Con un header Authorization si decide solo su quello: un token
		// sbagliato nello script non deve essere nascosto da un cookie valido.
		if h := r.Header.Get("Authorization"); h != "" {
			if t, ok := strings.CutPrefix(h, "Bearer "); ok && a.tokenOK(t) {
				next.ServeHTTP(w, withActor(r, actorToken))
				return
			}
		} else if c, err := r.Cookie(adminCookie); err == nil {
			if sess, ok := a.session(c.Value); ok {
				next.ServeHTTP(w, withActor(r, sess.name))
				return
			}
		}
		writeError(w, http.StatusUnauthorized, errCodeUnauthorized, "missing or invalid admin credentials")
	})
}

// adminLoginRequest è il corpo di POST /api/v1/admin/login.
type adminLoginRequest struct {
	Token string `json:"token"`
	// Name è chi sta entrando (per il registro delle modifiche): con un
	// token unico è l'unico modo di sapere chi ha cambiato cosa.
	Name string `json:"name"`
}

// adminLoginResponse è la risposta 200 del login.
type adminLoginResponse struct {
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleLogin scambia il token con un cookie di sessione.
//
// Esiti:
//
//	200 → adminLoginResponse, con Set-Cookie
//	400 invalid_request → JSON malformato o name vuoto
//	401 unauthorized    → token sbagliato
//
// È un metodo di Server, non di adminAuth, per riusare s.decodeJSON (limite
// del corpo, chiavi sconosciute rifiutate); l'adminAuth è s.admin.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	a := s.admin
	var req adminLoginRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "name is required")
		return
	}
	if !a.tokenOK(req.Token) {
		writeError(w, http.StatusUnauthorized, errCodeUnauthorized, "wrong admin token")
		return
	}
	id, err := a.newSession(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// HttpOnly: il JavaScript della pagina non lo legge (un XSS non lo ruba).
	// SameSite Strict: il browser non lo manda da altri siti. Secure solo con
	// TLS: in HTTP semplice il browser lo scarterebbe.
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int(adminSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
	writeJSON(w, http.StatusOK, adminLoginResponse{Name: name, ExpiresAt: a.now().Add(adminSessionTTL)})
}

// handleLogout chiude la sessione del cookie, se c'è.
//
//	204 sempre (anche senza cookie: il risultato è lo stesso)
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminCookie); err == nil {
		s.admin.mu.Lock()
		delete(s.admin.sessions, c.Value)
		s.admin.mu.Unlock()
	}
	// MaxAge -1: il browser cancella il cookie (stesso Name e Path).
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}
