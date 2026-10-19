package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/stream"
)

// --- Client ---------------------------------------------------------------------

func TestClientExecute(t *testing.T) {
	fake, addr, stop := serveFake(t, "127.0.0.1:0")
	defer stop()
	c, err := Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	job := stream.Job{ModelKey: "tiny", SplitPoint: 1, Tensor: []byte("T"),
		Deadline: time.Now().Add(time.Second), WantRaw: true}
	out, err := c.Execute(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Output) != "OUTPUT" || len(out.LayerMS) != 2 || out.QueueWaitMS != 0.5 {
		t.Errorf("outcome = %+v", out)
	}
	if !fake.sawDeadline || fake.lastRequest.GetSplitPoint() != 1 || fake.lastRequest.GetModelKey() != "tiny" {
		t.Errorf("richiesta al worker: deadline %v, %+v", fake.sawDeadline, fake.lastRequest)
	}

	job.WantRaw = false
	if out, _ := c.Execute(context.Background(), job); out.Output != nil {
		t.Error("senza WantRaw l'output non va restituito")
	}
}

func TestStatusToError(t *testing.T) {
	cases := []struct {
		code codes.Code
		want error
	}{
		{codes.DeadlineExceeded, stream.ErrDeadlineExceeded},
		{codes.NotFound, ErrInvalidJob},
		{codes.InvalidArgument, ErrInvalidJob},
		{codes.Unavailable, stream.ErrWorkerUnavailable},
		{codes.Internal, stream.ErrWorkerUnavailable},
		{codes.Canceled, stream.ErrWorkerUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			err := statusToError(grpcErr(tc.code))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), "detail from worker") {
				t.Errorf("il messaggio originale è andato perso: %v", err)
			}
		})
	}
	if err := statusToError(errors.New("not grpc")); !errors.Is(err, stream.ErrWorkerUnavailable) {
		t.Errorf("errore non gRPC = %v", err)
	}
}

func TestClientExecuteErrors(t *testing.T) {
	fake, addr, stop := serveFake(t, "127.0.0.1:0")
	defer stop()
	c, _ := Dial(addr)
	defer c.Close()

	fake.mu.Lock()
	fake.runErr = grpcErr(codes.DeadlineExceeded)
	fake.mu.Unlock()
	if _, err := c.Execute(context.Background(), stream.Job{ModelKey: "tiny"}); !errors.Is(err, stream.ErrDeadlineExceeded) {
		t.Errorf("err = %v", err)
	}

	stop() // worker spento
	_, err := c.Execute(context.Background(), stream.Job{ModelKey: "tiny", Deadline: time.Now().Add(500 * time.Millisecond)})
	if !errors.Is(err, stream.ErrWorkerUnavailable) && !errors.Is(err, stream.ErrDeadlineExceeded) {
		t.Errorf("worker spento: err = %v", err)
	}
}

// --- BuildCatalog -------------------------------------------------------------------

func TestBuildCatalog(t *testing.T) {
	models, mismatches := BuildCatalog(fakeDescription(), knownTiny)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v", mismatches)
	}
	want := []api.SegmentSize{{SplitPoint: -1, InputBytes: 24}, {SplitPoint: 0, InputBytes: 16}, {SplitPoint: 1, InputBytes: 16}}
	got := models["tiny"]
	if len(got) != len(want) {
		t.Fatalf("segments = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("segment %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestBuildCatalogMismatches(t *testing.T) {
	cases := []struct {
		name   string
		known  []api.ModelEntry
		reason string
	}{
		{"modello sconosciuto", nil, "config"},
		{"hash diverso", []api.ModelEntry{{ModelHash: "other", ModelInfo: api.ModelInfo{ModelKey: "tiny", NumLayers: 3}}}, fakeHash},
		{"numero di segmenti", []api.ModelEntry{{ModelHash: fakeHash, ModelInfo: api.ModelInfo{ModelKey: "tiny", NumLayers: 4}}}, "segment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, mismatches := BuildCatalog(fakeDescription(), tc.known)
			if _, ok := models["tiny"]; ok {
				t.Error("un modello incoerente non deve avere il data plane")
			}
			if len(mismatches) != 1 || mismatches[0].ModelKey != "tiny" ||
				!strings.Contains(mismatches[0].String(), tc.reason) {
				t.Errorf("mismatches = %v, want una ragione con %q", mismatches, tc.reason)
			}
		})
	}
}

func TestBuildCatalogModelNotServedIsNotAMismatch(t *testing.T) {
	known := append([]api.ModelEntry{{ModelHash: "h2", ModelInfo: api.ModelInfo{ModelKey: "yolo", NumLayers: 8}}}, knownTiny...)
	models, mismatches := BuildCatalog(fakeDescription(), known)
	if len(mismatches) != 0 || len(models) != 1 {
		t.Errorf("models %v, mismatches %v", models, mismatches)
	}
}

func TestCatalogReplaceConcurrent(t *testing.T) {
	c := NewCatalog()
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			c.Replace(map[string][]api.SegmentSize{"tiny": {{SplitPoint: 0, InputBytes: 16}}})
			c.Replace(nil)
		}
	})
	wg.Go(func() {
		for range 200 {
			c.Segments("tiny")
		}
	})
	wg.Wait()
	if _, ok := c.Segments("tiny"); ok {
		t.Error("dopo Replace(nil) il catalogo deve essere vuoto")
	}
}

// --- Supervisor -------------------------------------------------------------------

func helperOptions(t *testing.T, mode string) (Options, string) {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, mode)
	t.Setenv(helperEnv+"_STARTS", counter)
	return Options{
		Command:        []string{os.Args[0], "-test.run=^TestHelperWorker$", "--"},
		Address:        freeAddr(t),
		Models:         []ModelArg{{Key: "tiny", Dir: "/models/tiny"}},
		Slots:          2,
		Threads:        3,
		Known:          knownTiny,
		StartupTimeout: 5 * time.Second,
		MinBackoff:     50 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
		StopGrace:      300 * time.Millisecond,
		HealthInterval: 100 * time.Millisecond,
	}, counter
}

func runSupervisor(t *testing.T, opts Options) (*Supervisor, *Catalog, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	catalog := NewCatalog()
	sup, err := NewSupervisor(opts, catalog)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sup.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return sup, catalog, cancel, done
}

func waitDone(t *testing.T, done <-chan struct{}, within time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("Run non è terminato entro %v", within)
	}
}

func TestSupervisorStartsAndStopsTheWorker(t *testing.T) {
	opts, _ := helperOptions(t, "serve")
	sup, catalog, cancel, done := runSupervisor(t, opts)

	eventually(t, 10*time.Second, "worker pronto", sup.Ready)
	if s, ok := catalog.Segments("tiny"); !ok || len(s) != 3 {
		t.Errorf("catalogo = %v, %v", s, ok)
	}
	if n := sup.Slots(); n != 2 {
		t.Errorf("Slots = %d, attesi 2 (dal Describe)", n)
	}
	out, err := sup.Execute(context.Background(), stream.Job{ModelKey: "tiny", WantRaw: true,
		Deadline: time.Now().Add(time.Second)})
	if err != nil || string(out.Output) != "OUTPUT" {
		t.Errorf("Execute = %+v, %v", out, err)
	}

	cancel()
	waitDone(t, done, 3*time.Second)
	if sup.Ready() {
		t.Error("dopo lo spegnimento non deve essere pronto")
	}
	if _, ok := catalog.Segments("tiny"); ok {
		t.Error("dopo lo spegnimento il catalogo deve essere vuoto")
	}
	if n := sup.Slots(); n != 0 {
		t.Errorf("Slots dopo lo spegnimento = %d", n)
	}
}

func TestSupervisorRestartsACrashedWorker(t *testing.T) {
	opts, counter := helperOptions(t, "crash")
	sup, _, _, _ := runSupervisor(t, opts)

	eventually(t, 10*time.Second, "almeno due avvii", func() bool { return starts(counter) >= 2 })
	eventually(t, 10*time.Second, "di nuovo pronto dopo il crash", sup.Ready)
}

func TestSupervisorStartupTimeout(t *testing.T) {
	opts, counter := helperOptions(t, "never")
	opts.StartupTimeout = 300 * time.Millisecond
	sup, _, _, _ := runSupervisor(t, opts)

	eventually(t, 10*time.Second, "riavvio dopo il timeout di avvio", func() bool { return starts(counter) >= 2 })
	if sup.Ready() {
		t.Error("un worker che non risponde non deve risultare pronto")
	}
	if _, err := sup.Execute(context.Background(), stream.Job{ModelKey: "tiny"}); !errors.Is(err, stream.ErrWorkerUnavailable) {
		t.Errorf("Execute senza worker = %v", err)
	}
}

func TestSupervisorKillsAWorkerThatIgnoresSIGTERM(t *testing.T) {
	opts, _ := helperOptions(t, "ignore-term")
	sup, _, cancel, done := runSupervisor(t, opts)
	eventually(t, 10*time.Second, "worker pronto", sup.Ready)

	start := time.Now()
	cancel()
	waitDone(t, done, 5*time.Second)
	if elapsed := time.Since(start); elapsed < opts.StopGrace {
		t.Errorf("fermato in %v: doveva aspettare StopGrace (%v) prima di SIGKILL", elapsed, opts.StopGrace)
	}
}

func TestSupervisorExternalWorker(t *testing.T) {
	_, addr, stop := serveFake(t, "127.0.0.1:0")
	opts := Options{Address: addr, Known: knownTiny, StartupTimeout: 2 * time.Second, HealthInterval: 50 * time.Millisecond}
	sup, catalog, _, _ := runSupervisor(t, opts)

	eventually(t, 5*time.Second, "worker esterno pronto", sup.Ready)
	if _, ok := catalog.Segments("tiny"); !ok {
		t.Error("catalogo vuoto")
	}
	stop()
	eventually(t, 5*time.Second, "worker esterno perso", func() bool { return !sup.Ready() })
}
