package worker

import (
	"context"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/workerpb"
)

// --- fake worker (gRPC server in Go) ------------------------------------------

// fakeWorker è un PartitionWorkerServer con risposte programmabili.
type fakeWorker struct {
	workerpb.UnimplementedPartitionWorkerServer

	mu          sync.Mutex
	runErr      error
	sawDeadline bool
	lastRequest *workerpb.RunRequest
}

const fakeHash = "0123456789abcdef0123456789abcdef"

// fakeDescription: un modello "tiny" con 3 segmenti, input di 24, 16, 16 byte.
func fakeDescription() *workerpb.DescribeResponse {
	seg := func(i int32, in int64) *workerpb.Segment {
		return &workerpb.Segment{Index: i, InputShape: []int32{1, int32(in / 4)}, InputDtype: "float32",
			InputBytes: in, OutputShape: []int32{1, 4}, OutputBytes: 16}
	}
	return &workerpb.DescribeResponse{
		Slots: 2, Threads: 3,
		Models: []*workerpb.ModelDescription{{
			ModelKey: "tiny", ModelHash: fakeHash,
			Segments: []*workerpb.Segment{seg(0, 24), seg(1, 16), seg(2, 16)},
		}},
	}
}

func (f *fakeWorker) Describe(context.Context, *workerpb.DescribeRequest) (*workerpb.DescribeResponse, error) {
	return fakeDescription(), nil
}

func (f *fakeWorker) RunFrom(ctx context.Context, req *workerpb.RunRequest) (*workerpb.RunResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, f.sawDeadline = ctx.Deadline()
	f.lastRequest = req
	if f.runErr != nil {
		return nil, f.runErr
	}
	return &workerpb.RunResponse{Output: []byte("OUTPUT"), LayerMs: []float64{1, 2}, QueueWaitMs: 0.5}, nil
}

// serveFake avvia un fakeWorker su addr ("127.0.0.1:0" = porta libera).
func serveFake(t testing.TB, addr string) (*fakeWorker, string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeWorker{}
	srv := grpc.NewServer()
	workerpb.RegisterPartitionWorkerServer(srv, f)
	go srv.Serve(lis)
	return f, lis.Addr().String(), srv.Stop
}

// freeAddr restituisce un indirizzo 127.0.0.1:<porta libera>.
func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	return lis.Addr().String()
}

// --- helper process ---------------------------------------------------------------
//
// Il binario dei test, rilanciato con SCIOT_FAKE_WORKER=<modo>, si comporta
// da worker: è lo schema usato dai test di os/exec nella libreria standard.
// Il supervisore lo avvia come se fosse "uv run sciot-worker ...".

const helperEnv = "SCIOT_FAKE_WORKER"

func TestHelperWorker(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return // eseguito come test normale: non fa nulla
	}
	countStart()

	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	listen := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--listen" {
			listen = args[i+1]
		}
	}

	switch mode {
	case "never": // non risponde mai: per il timeout di avvio
		time.Sleep(time.Minute)
		os.Exit(0)
	case "ignore-term": // ignora SIGTERM: deve arrivare SIGKILL
		signal.Ignore(syscall.SIGTERM)
	}

	_, _, stop := serveFake(t, listen)
	switch mode {
	case "crash": // pronto per un po', poi muore
		time.Sleep(400 * time.Millisecond)
		os.Exit(3)
	case "ignore-term":
		// Non arrivare a signal.Notify qui sotto: riattiverebbe SIGTERM
		// (annulla signal.Ignore) e il worker uscirebbe subito.
		select {}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	<-sig
	stop()
	os.Exit(0)
}

// countStart aggiunge una riga al file SCIOT_FAKE_WORKER_STARTS: il test
// conta così quante volte il worker è stato avviato.
func countStart() {
	path := os.Getenv(helperEnv + "_STARTS")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
		f.Close()
	}
}

func starts(path string) int {
	data, _ := os.ReadFile(path)
	return strings.Count(string(data), "\n")
}

var knownTiny = []api.ModelEntry{{ModelHash: fakeHash, ModelInfo: api.ModelInfo{ModelKey: "tiny", NumLayers: 3, LastOffloadingLayer: 2}}}

// eventually ripete check finché è vero o scade timeout.
func eventually(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func grpcErr(c codes.Code) error { return status.Error(c, "detail from worker") }
