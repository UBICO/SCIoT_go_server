package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/stream"
)

// ModelArg è un modello da caricare nel worker.
type ModelArg struct {
	Key string
	Dir string
}

// Options configura il Supervisor.
type Options struct {
	// Command avvia il worker, senza gli argomenti che aggiunge il supervisore
	// (--listen, --slots, --threads, --model). Esempio:
	//   ["uv", "run", "--project", "worker", "sciot-worker"]
	// Vuoto = worker esterno: il supervisore non avvia processi, si collega
	// ad Address e ne controlla lo stato.
	Command []string
	Address string // host:port del worker, es. 127.0.0.1:50051

	Models  []ModelArg
	Slots   int
	Threads int

	// Known sono i modelli del registry, per verificare la descrizione del worker.
	Known []api.ModelEntry

	StartupTimeout time.Duration // tempo massimo perché il worker diventi pronto
	MinBackoff     time.Duration // prima attesa tra due tentativi di avvio
	MaxBackoff     time.Duration // attesa massima (raddoppia fino a qui)
	StopGrace      time.Duration // dopo SIGTERM, SIGKILL se non è ancora uscito
	HealthInterval time.Duration // solo worker esterno: ogni quanto controllarlo
}

// Supervisor gestisce il ciclo di vita del worker e fa da stream.Executor:
// finché il worker non è pronto, ogni job riceve stream.ErrWorkerUnavailable
// e il device esegue quel frame LOCAL.
type Supervisor struct {
	opts    Options
	catalog *Catalog

	// mu protegge i due campi sotto. client cambia a ogni avvio del worker
	// gestito: una connessione nuova per ogni processo nuovo, così non si
	// eredita l'attesa di riconnessione della connessione al processo morto.
	mu     sync.Mutex
	client *Client
	ready  bool
	slots  int // dal Describe: vale anche per un worker esterno
}

var _ stream.Executor = (*Supervisor)(nil)

// NewSupervisor crea un Supervisor. Il worker parte solo con Run.
func NewSupervisor(opts Options, catalog *Catalog) (*Supervisor, error) {
	client, err := Dial(opts.Address)
	if err != nil {
		return nil, err
	}
	return &Supervisor{opts: opts, client: client, catalog: catalog}, nil
}

// Execute implementa stream.Executor.
func (s *Supervisor) Execute(ctx context.Context, job stream.Job) (stream.Outcome, error) {
	s.mu.Lock()
	client, ready := s.client, s.ready
	s.mu.Unlock()
	if !ready {
		return stream.Outcome{}, fmt.Errorf("%w: not ready", stream.ErrWorkerUnavailable)
	}
	return client.Execute(ctx, job)
}

// currentClient restituisce il client della connessione in uso.
func (s *Supervisor) currentClient() *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// reconnect sostituisce il client con uno nuovo e chiude il precedente.
func (s *Supervisor) reconnect() error {
	client, err := Dial(s.opts.Address)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := s.client
	s.client = client
	s.mu.Unlock()
	return old.Close()
}

// Slots restituisce i job che il worker esegue in parallelo, 0 se non è
// pronto. Il dispatcher non gliene manda mai di più.
func (s *Supervisor) Slots() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slots
}

// Ready riporta se il worker ha risposto a Describe ed è in uso.
func (s *Supervisor) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// setReady aggiorna lo stato e il catalogo insieme: un worker non pronto non
// deve avere modelli nel catalogo (lo stream rifiuterebbe comunque i frame,
// ma con 503 già all'apertura dello stream è più chiaro per il device).
func (s *Supervisor) setReady(models map[string][]api.SegmentSize, slots int) {
	s.mu.Lock()
	s.ready = models != nil
	s.slots = 0
	if s.ready {
		s.slots = slots
	}
	s.mu.Unlock()
	s.catalog.Replace(models)
}

// Run gestisce il worker finché ctx non viene cancellato. Va avviato in una
// goroutine: go sup.Run(ctx). Restituisce quando il worker è stato fermato.
func (s *Supervisor) Run(ctx context.Context) {
	defer func() { s.currentClient().Close() }()
	if len(s.opts.Command) == 0 {
		s.runExternal(ctx)
		return
	}
	s.runManaged(ctx)
}

// runManaged avvia il processo e lo riavvia quando esce, con attesa crescente.
func (s *Supervisor) runManaged(ctx context.Context) {
	// Attesa crescente: se il worker muore subito all'avvio (modello mancante,
	// porta occupata), riavviarlo senza pausa riempirebbe il log e la CPU.
	backoff := s.opts.MinBackoff
	for {
		exited, stop, err := s.start(ctx)
		if err != nil {
			log.Printf("worker: %v", err)
		} else {
			if err := s.waitReady(ctx); err == nil {
				backoff = s.opts.MinBackoff // avvio riuscito: azzera l'attesa
			} else if ctx.Err() == nil {
				// Non è partito in tempo: lo si ferma e si riprova.
				log.Printf("worker: %v; restarting", err)
				stop()
			}

			select {
			case err := <-exited:
				s.setReady(nil, 0)
				log.Printf("worker exited: %v", err)
			case <-ctx.Done():
				s.setReady(nil, 0)
				stop()
				<-exited
				return
			}
		}

		log.Printf("worker: restarting in %v", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, s.opts.MaxBackoff)
	}
}

// runExternal si collega a un worker avviato da altri e ne controlla lo stato.
func (s *Supervisor) runExternal(ctx context.Context) {
	ticker := time.NewTicker(s.opts.HealthInterval)
	defer ticker.Stop()
	for {
		if !s.Ready() {
			if err := s.waitReady(ctx); err != nil && ctx.Err() == nil {
				log.Printf("worker %s not ready: %v", s.opts.Address, err)
			}
		} else if _, err := s.currentClient().Describe(ctx); err != nil && ctx.Err() == nil {
			log.Printf("worker %s lost: %v", s.opts.Address, err)
			s.setReady(nil, 0)
		}
		select {
		case <-ctx.Done():
			s.setReady(nil, 0)
			return
		case <-ticker.C:
		}
	}
}

// waitReady chiama Describe finché il worker risponde o scade StartupTimeout.
// Quando risponde costruisce il catalogo e segna il worker come pronto.
func (s *Supervisor) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.StartupTimeout)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		// I primi Describe falliscono con Unavailable mentre il worker importa
		// TensorFlow e carica i modelli: è normale, non si logga.
		if desc, err := s.currentClient().Describe(ctx); err == nil {
			models, mismatches := BuildCatalog(desc, s.opts.Known)
			for _, m := range mismatches {
				log.Printf("worker %s: %s", s.opts.Address, m)
			}
			log.Printf("worker ready: %d models enabled, %d slots, %d threads",
				len(models), desc.GetSlots(), desc.GetThreads())
			s.setReady(models, int(desc.GetSlots()))
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker not ready after %v: %w", s.opts.StartupTimeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// start avvia il processo del worker e restituisce:
//   - exited: riceve il risultato di Wait quando il processo termina;
//   - stop: chiede al processo di fermarsi (SIGTERM al gruppo, SIGKILL dopo
//     StopGrace). Si può chiamare più volte.
func (s *Supervisor) start(ctx context.Context) (exited <-chan error, stop func(), err error) {
	args := append([]string{}, s.opts.Command[1:]...)
	args = append(args,
		"--listen", s.opts.Address,
		"--slots", strconv.Itoa(s.opts.Slots),
		"--threads", strconv.Itoa(s.opts.Threads),
	)
	for _, m := range s.opts.Models {
		args = append(args, "--model", m.Key+"="+m.Dir)
	}

	// Un context per QUESTO processo: cancellarlo lo ferma. Deriva da ctx,
	// quindi lo spegnimento del server ferma anche il worker.
	procCtx, stop := context.WithCancel(ctx)
	cmd := exec.CommandContext(procCtx, s.opts.Command[0], args...)

	// Un gruppo di processi proprio: "uv run" avvia python come processo
	// figlio, e il segnale va mandato a tutto il gruppo (pid negativo).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Alla cancellazione di procCtx Go chiama Cancel: di default sarebbe
	// SIGKILL; noi chiediamo un'uscita ordinata con SIGTERM.
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	// Se dopo Cancel il processo non esce entro WaitDelay, Go lo uccide
	// (SIGKILL) e chiude le pipe: Wait non resta bloccato per sempre.
	cmd.WaitDelay = s.opts.StopGrace

	// stdout e stderr nel log del server, riga per riga. Con un io.Pipe
	// nostro (invece di StderrPipe) è Go a copiare l'output, e WaitDelay
	// copre anche un processo figlio che tiene la pipe aperta.
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	go forwardLines(pr, "worker | ")

	if err := cmd.Start(); err != nil {
		stop()
		pw.Close()
		return nil, nil, fmt.Errorf("start worker: %w", err)
	}
	log.Printf("worker started: pid %d, %s", cmd.Process.Pid, s.opts.Address)
	if err := s.reconnect(); err != nil {
		log.Printf("worker: reconnect: %v", err)
	}

	ch := make(chan error, 1) // bufferizzato: la goroutine non resta bloccata
	go func() {
		err := cmd.Wait()
		pw.Close() // fine dell'output: forwardLines termina
		stop()     // libera procCtx
		ch <- err
	}()
	return ch, stop, nil
}

// forwardLines copia ogni riga di r nel log del server, con un prefisso.
func forwardLines(r io.Reader, prefix string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		log.Print(prefix + scanner.Text())
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("%sread error: %v", prefix, err)
	}
}
