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
	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/offload"
	"github.com/UBICO/SCIoT_go_server/internal/record"
	"github.com/UBICO/SCIoT_go_server/internal/registry"
	"github.com/UBICO/SCIoT_go_server/internal/server"
	"github.com/UBICO/SCIoT_go_server/internal/stream"
	"github.com/UBICO/SCIoT_go_server/internal/worker"
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
	segmentTau, err := f.DeviceSegmentTau()
	if err != nil {
		log.Fatalf("server config: %v", err)
	}
	devices.SetSegmentTau(segmentTau)

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

	// Algoritmo di offloading: "static" (default) lascia la policy all'admin,
	// senza calcolare previsioni a ogni heartbeat.
	algName, algCfg, err := f.OffloadConfig()
	if err != nil {
		log.Fatalf("offload config: %v", err)
	}
	if algName != (offload.Static{}).Name() {
		alg, err := offload.New(algName, algCfg)
		if err != nil {
			log.Fatalf("offload: %v", err)
		}
		srv.SetAlgorithm(alg)
		log.Printf("offloading algorithm: %s (objective %s)", algName, algCfg.Objective)
	}

	// --- 3b. Data plane (solo se config.json ha la sezione "worker") ---------

	// streamHandler resta nil senza data plane: lo shutdown lo controlla.
	var streamHandler *stream.Handler
	// workerDone viene chiuso quando sup.Run è terminato, cioè quando il
	// processo del worker è uscito. nil senza data plane: il main non aspetta.
	var workerDone chan struct{}
	dispatchCfg, err := f.DispatchConfig()
	if err != nil {
		log.Fatalf("dispatch config: %v", err)
	}

	workerOpts, dataPlane, err := f.WorkerOptions(reg.Models())
	if err != nil {
		log.Fatalf("worker config: %v", err)
	}
	streamCfg, err := f.StreamConfig()
	if err != nil {
		log.Fatalf("stream config: %v", err)
	}
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

	recordDir, recording, err := f.RecordDir()
	if err != nil {
		log.Fatalf("record config: %v", err)
	}
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
		if streamHandler != nil {
			streamHandler.SetRecorder(rec)
		}
		log.Printf("recording to %s", rec.Stats().File)
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
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	// Dopo Shutdown: nessuna richiesta registra più nulla. Close scrive gli
	// eventi in attesa e la riga "end".
	if rec != nil {
		if err := rec.Close(); err != nil {
			log.Printf("record: %v", err)
		}
		stats := rec.Stats() // dopo Close: conta anche gli eventi che erano in coda
		log.Printf("recorded %d lines to %s (%d events dropped)", stats.Written, stats.File, stats.Dropped)
	}
}
