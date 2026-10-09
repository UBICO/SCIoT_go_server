// Il package "main" è il programma eseguibile: assembla i package interni
// e avvia il server HTTP. Non contiene logica propria.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/UBICO/sciot-go-server/internal/device"
	"github.com/UBICO/sciot-go-server/internal/registry"
	"github.com/UBICO/sciot-go-server/internal/server"
)

func main() {
	// --- 1. Argomenti da riga di comando ------------------------------------

	// flag.String registra un argomento e restituisce un *string: il valore
	// vero arriva solo dopo flag.Parse(), quindi si legge con *configPath.
	//
	//   go run ./cmd/server -config config.json -addr :23456
	//   go run ./cmd/server -h      ← stampa l'help generato automaticamente
	configPath := flag.String("config", "config.json", "path del file di configurazione JSON")
	addr := flag.String("addr", ":8080", "address of the sciot-server")
	flag.Parse()

	// --- 2. Registry dei modelli ---------------------------------------------

	//     se err != nil → log.Fatalf("load config: %v", err)
	//     log.Fatalf stampa ed esce con codice 1: il server non deve partire
	//     con una configurazione rotta.
	cfg, err := registry.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	//     Nota: qui err esiste già, quindi si usa = e non :=. Però := è
	//     ammesso se ALMENO UNA variabile a sinistra è nuova (reg lo è).
	reg, err := registry.New(cfg)
	if err != nil {
		log.Fatalf("build registry: %v", err)
	}
	log.Printf("loaded %d models", reg.Len())

	// --- 3. Store e Server ---------------------------------------------------

	devices := device.NewStore()
	srv := server.New(reg, devices, server.DefaultConfig())

	// --- 4. Server HTTP con timeout ------------------------------------------

	// http.ListenAndServe(addr, handler) usa un http.Server senza timeout:
	// un client che apre una connessione e non invia nulla la tiene
	// occupata per sempre. Si costruisce quindi un http.Server esplicito.
	//
	// completa il letterale:
	//   - Addr:    *addr
	//   - Handler: srv.Routes()
	httpServer := &http.Server{
		Addr: *addr,
		// ReadHeaderTimeout: tempo massimo per ricevere gli header.
		ReadHeaderTimeout: 5 * time.Second,
		// ReadTimeout / WriteTimeout: tempo massimo per leggere la richiesta
		// intera e scrivere la risposta. Per il control plane i messaggi
		// sono piccoli: 10 s è largo. Il data plane (activation) potrebbe
		// richiedere valori diversi: lo rivedremo.
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		// IdleTimeout: quanto resta aperta una connessione keep-alive tra
		// una richiesta e l'altra. È ciò che permette al client di
		// RIUSARE la connessione (§7 e §18 del report): con heartbeat a
		// 1 Hz la connessione non va mai in idle. 120 s come
		// timeout_keep_alive del server Python.
		IdleTimeout: 120 * time.Second,
		Handler:     srv.Routes(),
	}

	log.Printf("listening on %s", httpServer.Addr)
	log.Fatal(httpServer.ListenAndServe())
}
