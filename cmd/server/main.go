// Il package "main" è il programma eseguibile: assembla i package interni
// e avvia il server HTTP. Non contiene logica propria.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/config"
	"github.com/UBICO/SCIoT_go_server/internal/device"
	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/record"
	"github.com/UBICO/SCIoT_go_server/internal/server"
	"github.com/UBICO/SCIoT_go_server/internal/stream"
	"github.com/UBICO/SCIoT_go_server/internal/worker"
)

// startEnv è l'ambiente all'avvio, SCIOT_ADMIN_TOKEN compreso: run lo toglie
// da os.Environ, il riavvio lo ridà al nuovo programma.
var startEnv = os.Environ()

func main() {
	if !run() {
		return // Ctrl+C o SIGTERM: il server esce, come sempre
	}
	// Riavvio chiesto dalla dashboard (POST /api/v1/admin/restart). Qui i
	// defer di run sono già stati eseguiti: worker fermo, registrazione e
	// registro chiusi, porte libere.
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("restart: %v", err)
	}
	log.Printf("restarting: %s", strings.Join(os.Args, " "))
	// Exec sostituisce il programma nello stesso processo: stesso PID, stessi
	// argomenti e ambiente (-config, SCIOT_ADMIN_TOKEN). Se ritorna, è fallito.
	err = syscall.Exec(exe, os.Args, startEnv)
	log.Fatalf("restart: %v", err)
}

// run è il server: avvia tutto, aspetta un segnale o un riavvio, spegne
// tutto in ordine. Restituisce true se lo spegnimento era per un riavvio:
// main allora rilancia il programma, dopo che tutti i defer di run sono
// stati eseguiti (per questo il corpo non sta più in main).
func run() bool {
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
	// Vuoto = nessuna porta admin né dashboard: il server è quello di prima.
	adminAddr := flag.String("admin-addr", "", "address of the admin API and dashboard, e.g. 127.0.0.1:8081 (token: SCIOT_ADMIN_TOKEN)")
	flag.Parse()

	// --- 2. Configurazione e registry -----------------------------------------

	// log.Fatalf stampa ed esce con codice 1: il server non deve partire
	// con una configurazione rotta. Ogni messaggio dice quale passo è fallito.
	f, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	// Tutti i controlli della config, gli stessi della dashboard
	// (cmd/server/config.go). L'errore ha già il prefisso del passo fallito.
	p, err := parseConfig(f)
	if err != nil {
		log.Fatalf("%v", err)
	}

	reg := p.reg
	log.Printf("loaded %d models", reg.Len())

	srvCfg := p.server
	if *verbose {
		srvCfg.Verbose = true
	}
	if *admin {
		srvCfg.Admin = true
	}
	if srvCfg.Admin {
		log.Printf("WARNING: admin endpoint enabled without authentication: bind to localhost (-addr 127.0.0.1:PORT)")
	}
	// Il token viene dall'ambiente, mai da config.json: il file finisce nei
	// backup e magari in git, una variabile d'ambiente no.
	adminToken := os.Getenv("SCIOT_ADMIN_TOKEN")
	// Tolto dall'ambiente: i processi figli (worker, build) lo ereditano e non
	// ne hanno bisogno. Il riavvio usa startEnv, salvato prima di run.
	os.Unsetenv("SCIOT_ADMIN_TOKEN")
	if *adminAddr != "" {
		if err := server.CheckAdminAddr(*adminAddr, adminToken); err != nil {
			log.Fatalf("admin: %v", err)
		}
		if adminToken == "" {
			log.Printf("WARNING: admin API on %s without authentication (loopback only): set SCIOT_ADMIN_TOKEN to require a token", *adminAddr)
		}
		// -admin apre gli stessi endpoint senza autenticazione sulla porta dei
		// device: il token della porta admin non protegge più la policy.
		if srvCfg.Admin {
			log.Printf("WARNING: -admin is also on: the policy endpoints are open without authentication on %s, the admin token does not protect them", *addr)
		}
	}
	log.Printf("heartbeat interval %v, verbose %v", srvCfg.HeartbeatInterval, srvCfg.Verbose)

	// --- 3. Store, monitor del carico e Server -------------------------------

	devices := device.NewStore()
	devices.SetSegmentTau(p.segmentTau)

	loadCfg := p.load
	if loadCfg.Interval > srvCfg.HeartbeatInterval {
		log.Printf("WARNING: load sample interval %v is longer than heartbeat interval %v: each heartbeat will see a sample older than one interval",
			loadCfg.Interval, srvCfg.HeartbeatInterval)
	}

	monitor := load.NewMonitor(load.HostReader{}, loadCfg)

	// Il context viene cancellato al primo Ctrl+C o SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Il secondo motivo per spegnersi, il riavvio: cancellare ctx è come un
	// Ctrl+C, lo stesso spegnimento. restart lo scrive l'handler admin, lo
	// legge run alla fine.
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var restart atomic.Bool

	go monitor.Run(ctx)

	srv := server.New(reg, devices, srvCfg, monitor)

	// Algoritmo di offloading: "static" (default) lascia la policy all'admin,
	// senza calcolare previsioni a ogni heartbeat.
	if p.alg != nil {
		srv.SetAlgorithm(p.alg)
		log.Printf("offloading algorithm: %s (objective %s)", p.algName, p.algCfg.Objective)
	}

	// --- 3b. Data plane (solo se config.json ha la sezione "worker") ---------

	// streamHandler resta nil senza data plane: lo shutdown lo controlla.
	var streamHandler *stream.Handler
	// workerDone viene chiuso quando sup.Run è terminato, cioè quando il
	// processo del worker è uscito. nil senza data plane: il main non aspetta.
	var workerDone chan struct{}
	dispatchCfg := p.dispatch
	workerOpts, dataPlane := p.worker, p.dataPlane
	streamCfg := p.stream
	// dispatcher, catalog e profileReady servono anche dopo la registrazione
	// (3c): il profilo registra i suoi eventi.
	var dispatcher *dispatch.Dispatcher
	var catalog *worker.Catalog
	profileReady := make(chan struct{}, 1)
	if dataPlane {
		catalog = worker.NewCatalog()
		workerOpts.OnReady = func() { signalReady(profileReady) }
		sup, err := worker.NewSupervisor(workerOpts, catalog)
		if err != nil {
			log.Fatalf("worker supervisor: %v", err)
		}
		// Run avvia il worker, lo riavvia se muore e lo ferma quando ctx viene
		// cancellato; workerDone dice al main che il processo è uscito.
		workerDone = make(chan struct{})
		go func() {
			defer close(workerDone)
			sup.Run(ctx)
		}()

		// Il dispatcher sta davanti al Supervisor: ammissione per scadenza,
		// coda FIFO, mai più job in volo degli slot del worker.
		dispatcher = dispatch.New(sup, dispatchCfg)
		srv.SetQueue(dispatcher)
		streamHandler = stream.NewHandler(server.Sessions(devices), catalog, dispatcher, streamCfg)
		// Prima di srv.Routes(), che legge la rotta una volta sola.
		srv.SetStream(streamHandler, streamHandler)

		mode := "external"
		if len(workerOpts.Command) > 0 {
			mode = "managed"
		}
		log.Printf("data plane enabled: worker %s (%s)", workerOpts.Address, mode)
	} else {
		log.Printf("data plane disabled: no \"worker\" section in config")
	}

	// --- 3c. Registrazione (solo se config.json ha la sezione "record") ------

	recordDir, recording := p.recordDir, p.recording
	var rec *record.Recorder // nil = spenta: chi registra non controlla
	if recording {
		start := record.Start{
			HeartbeatIntervalMS: int(srvCfg.HeartbeatInterval.Milliseconds()),
			Models:              reg.Models(),
		}
		if dataPlane {
			start.Dispatch = &record.DispatchConfig{
				ServiceTauMS: float64(dispatchCfg.ServiceTau.Milliseconds()),
				WindowS:      dispatchCfg.Window.Seconds(),
			}
		}
		rec, err = record.Open(recordDir, start, record.DefaultOptions())
		if err != nil {
			log.Fatalf("record: %v", err)
		}
		srv.SetRecorder(rec)
		srv.SetRecordDir(recordDir) // per l'elenco della dashboard e il registro
		if streamHandler != nil {
			streamHandler.SetRecorder(rec)
		}
		log.Printf("recording to %s", rec.Stats().File)
		go runtimeLoop(ctx, rec, devices, record.RuntimeInterval)

		// Il registro delle modifiche sta accanto alle registrazioni, un
		// file solo per tutti gli avvii (internal/server/audit.go).
		audit, err := server.OpenAudit(recordDir)
		if err != nil {
			log.Fatalf("audit: %v", err)
		}
		defer audit.Close()
		srv.SetAudit(audit)
		log.Printf("change log: %s", filepath.Join(recordDir, server.AuditFile))
	}

	// Il profilo parte dopo la registrazione, così ne registra gli eventi: un
	// worker già pronto ha lasciato il segnale nel buffer di profileReady.
	if dataPlane && dispatchCfg.ProfileRuns > 0 {
		go profileLoop(ctx, dispatcher, catalog, profileReady, dispatchCfg.ProfileRuns, dispatchCfg.ReprofileAge, rec)
	}

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
		// sono piccoli: 10 s è largo. Non limitano lo stream del data plane:
		// dopo l'upgrade la connessione WebSocket esce da http.Server
		// (verificato da TestStreamOutlivesHTTPServerTimeouts).
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

	// --- 4b. Porta admin (solo con -admin-addr) --------------------------------

	// adminServer resta nil senza -admin-addr: lo shutdown lo controlla.
	var adminServer *http.Server
	if *adminAddr != "" {
		// config.json dalla dashboard, con i controlli dell'avvio
		// (cmd/server/config.go). Prima di AdminRoutes.
		if err := srv.SetConfigFile(*configPath, checkConfig); err != nil {
			log.Fatalf("admin config: %v", err)
		}
		// Riavvio dalla dashboard; prima di AdminRoutes, come SetConfigFile.
		srv.SetRestart(func() { restart.Store(true); cancelRun() })
		// Build dei modelli dalla dashboard, solo con models_dir.
		if p.building {
			srv.SetModelBuilds(p.buildDir)
			log.Printf("model builds into %s", p.buildDir)
		}
		// Stessi timeout di httpServer: per ora l'API admin ha solo richieste
		// brevi (la SSE della PR 2 li rivedrà). AdminRoutes va dopo SetStream/
		// SetAlgorithm/SetRecorder, come Routes.
		adminServer = &http.Server{
			Addr:              *adminAddr,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       120 * time.Second,
			Handler:           srv.AdminRoutes(adminToken),
			// Il context del segnale: ogni richiesta admin ne deriva il suo
			// (r.Context()), quindi al Ctrl+C lo stream di eventi esce subito
			// invece di far aspettare a Shutdown tutti i 5 s.
			BaseContext: func(net.Listener) context.Context { return ctx },
		}
		go func() {
			log.Printf("admin API listening on %s", adminServer.Addr)
			if err := adminServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("admin server: %v", err)
			}
		}()
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
	// Shutdown non chiude le connessioni WebSocket (sono "hijacked": non più
	// gestite da http.Server) e aspetterebbe fino al timeout. Si chiudono
	// prima, con 1001 going away: il device sa che può riconnettersi.
	if streamHandler != nil {
		streamHandler.CloseAll()
	}
	defer func() {
		// Senza questa attesa il main uscirebbe mentre il worker sta ancora
		// ricevendo SIGTERM: il processo Python resterebbe orfano.
		if workerDone != nil {
			<-workerDone
			log.Printf("worker stopped")
		}
	}()
	// Le due porte si chiudono in parallelo, ognuna con tutto shutdownCtx: in
	// sequenza, una porta dei device lenta lascerebbe alla porta admin un
	// context già scaduto. wg.Go (Go 1.25) è wg.Add(1) + go + defer wg.Done().
	var wg sync.WaitGroup
	if adminServer != nil {
		wg.Go(func() {
			if err := adminServer.Shutdown(shutdownCtx); err != nil {
				log.Printf("admin shutdown: %v", err)
			}
		})
	}
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	// Prima di srv.Close: un pin dall'admin in corso deve finire prima che si
	// chiuda la registrazione.
	wg.Wait()
	// Nessun heartbeat nuovo: si ferma la goroutine delle decisioni, che
	// scrive nella registrazione.
	srv.Close()
	// Dopo Shutdown: nessuna richiesta registra più nulla. Close scrive gli
	// eventi in attesa e la riga "end".
	if rec != nil {
		if err := rec.Close(); err != nil {
			log.Printf("record: %v", err)
		}
		stats := rec.Stats() // dopo Close: conta anche gli eventi che erano in coda
		log.Printf("recorded %d lines to %s (%d events dropped)", stats.Written, stats.File, stats.Dropped)
	}
	return restart.Load()
}
