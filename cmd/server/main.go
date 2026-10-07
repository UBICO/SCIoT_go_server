// Il package "main" è speciale: indica un programma eseguibile.
// La funzione main() qui sotto è il punto d'ingresso.
package main

// Gli import usano il path completo del package.
// Questi sono tutti della libreria standard: niente da installare.
import (
	"encoding/json"
	"log"
	"net/http"
)

// healthResponse è il corpo JSON di GET /healthz.
//
// Il tag `json:"status"` dice al package encoding/json come chiamare
// il campo nel JSON. Il campo Status inizia con la maiuscola: è
// "esportato", e solo i campi esportati vengono serializzati.
type healthResponse struct {
	Status string `json:"status"`
}

// handleHealthz risponde a GET /healthz con {"status":"ok"}.
//
// Firma standard di un handler HTTP:
//   - w http.ResponseWriter: dove scrivi header, status code e corpo.
//   - r *http.Request: la richiesta (qui non ci serve).
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	// imposta l'header "Content-Type" a "application/json".
	//   Suggerimento: w.Header().Set(chiave, valore)
	w.Header().Set("Content-Type", "application/json")

	// imposta lo status code 200.
	//   Suggerimento: w.WriteHeader(...) — esiste la costante http.StatusOK.
	//   Attenzione: va chiamato DOPO aver impostato gli header,
	//   altrimenti gli header non vengono più inviati.
	w.WriteHeader(http.StatusOK)
	// crea un valore healthResponse con Status "ok" e scrivilo
	//   come JSON su w.
	//   Suggerimento: json.NewEncoder(w).Encode(valore) restituisce un error.
	//   Se err != nil, registralo con log.Printf("...: %v", err).
	//   (Non puoi più cambiare lo status code: è già stato inviato.)
	hResponse := healthResponse{Status: "ok"}
	err := json.NewEncoder(w).Encode(hResponse)
	if err != nil {
		log.Printf("Error: %v", err)
	}

}

func main() {
	// Il router: associa "METODO /path" a un handler.
	mux := http.NewServeMux()

	// registra handleHealthz sul pattern "GET /healthz".
	//   Suggerimento: mux.HandleFunc(pattern, funzione)

	mux.HandleFunc("GET /healthz", handleHealthz)

	addr := ":8080"
	log.Printf("server in ascolto su %s", addr)

	// ListenAndServe blocca finché il server gira e restituisce un error
	// solo se qualcosa va storto (es. porta già occupata).
	// log.Fatal stampa l'errore ed esce dal programma con codice 1.
	log.Fatal(http.ListenAndServe(addr, mux))
}
