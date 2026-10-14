// Il package "main" è il programma eseguibile: assembla i package interni
// e avvia il server HTTP. Non contiene logica propria.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/config"
	"github.com/UBICO/SCIoT_go_server/internal/device"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/registry"
	"github.com/UBICO/SCIoT_go_server/internal/server"
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
	// flag.Bool non distingue "non passato" da "false": il flag può solo
	// ATTIVARE la tabella; per disattivarla si usa config.json.
	verbose := flag.Bool("verbose", false, "stampa una riga per ogni heartbeat (sovrascrive config.json)")
	admin := flag.Bool("admin", false, "abilita l'endpoint admin per forzare la policy (senza autenticazione)")
	flag.Parse()

	// --- 2. Configurazione e registry -----------------------------------------

	// log.Fatalf stampa ed esce con codice 1: il server non deve partire
	// con una configurazione rotta. Ogni messaggio dice quale passo è fallito.
	f, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	reg, err := registry.New(f.RegistryConfig())
	if err != nil {
		log.Fatalf("build registry: %v", err)
	}
	log.Printf("loaded %d models", reg.Len())

	srvCfg, err := f.ServerConfig()
	if err != nil {
		log.Fatalf("server config: %v", err)
	}
	if *verbose {
		srvCfg.Verbose = true
	}
	if *admin {
		srvCfg.Admin = true
	}
	if srvCfg.Admin {
		log.Printf("WARNING: admin endpoint enabled without authentication: bind to localhost (-addr 127.0.0.1:PORT)")
	}
	log.Printf("heartbeat interval %v, verbose %v", srvCfg.HeartbeatInterval, srvCfg.Verbose)

	// --- 3. Store, monitor del carico e Server -------------------------------

	devices := device.NewStore()

	loadCfg, err := f.LoadConfig()
	if err != nil {
		log.Fatalf("load monitor config: %v", err)
	}
	if loadCfg.Interval > srvCfg.HeartbeatInterval {
		log.Printf("WARNING: load sample interval %v is longer than heartbeat interval %v: each heartbeat will see a sample older than one interval",
			loadCfg.Interval, srvCfg.HeartbeatInterval)
	}

	monitor := load.NewMonitor(load.HostReader{}, loadCfg)

	// Il context viene cancellato al primo Ctrl+C o SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go monitor.Run(ctx)

	srv := server.New(reg, devices, srvCfg, monitor)

	// --- 4. Server HTTP con timeout ------------------------------------------

	// http.ListenAndServe(addr, handler) usa un http.Server senza timeout:
	// un client che apre una connessione e non invia nulla la tiene
	// occupata per sempre. Si costruisce quindi un http.Server esplicito.
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

	// --- 5. Avvio e shutdown ordinato ----------------------------------------

	log.Printf("listening on %s", httpServer.Addr)

	// ListenAndServe blocca: va in una goroutine, così il main può aspettare
	// il segnale. Dopo Shutdown restituisce http.ErrServerClosed, che è l'esito
	// normale e non va trattato come errore.
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")

	// ctx è già cancellato: il contesto dello shutdown parte da Background,
	// altrimenti Shutdown scadrebbe subito.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
