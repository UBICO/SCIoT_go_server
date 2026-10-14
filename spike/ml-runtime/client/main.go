// Command client misura il worker gRPC (spike 2.2).
//
//	go run ./client -addr unix:///tmp/sciot-w0.sock -split 0 \
//	    -input out/input_k0.bin -expected out/expected_k0.bin -concurrency 4
//
// -addr accetta più indirizzi separati da virgola (pool di processi): le
// richieste vengono distribuite a turno (round-robin) tra i worker.
//
// Stampa una riga JSON con i risultati, così lo script può raccoglierli.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/UBICO/SCIoT_go_server/spike/ml-runtime/workerpb"
)

const maxMessageBytes = 64 << 20 // come MAX_MESSAGE_BYTES nel worker

// Result è la riga JSON stampata alla fine.
type Result struct {
	Variant       string  `json:"variant"` // es. "grpc-unix", "grpc-tcp"
	Workers       int     `json:"workers"` // numero di indirizzi (processi)
	Split         int     `json:"split"`
	Concurrency   int     `json:"concurrency"`
	N             int     `json:"n"` // richieste misurate (warmup escluso)
	InputBytes    int     `json:"input_bytes"`
	ThroughputRPS float64 `json:"throughput_rps"` // richieste completate al secondo
	MeanMS        float64 `json:"mean_ms"`        // latenza vista dal client
	P50MS         float64 `json:"p50_ms"`
	P95MS         float64 `json:"p95_ms"`
	QueueWaitMS   float64 `json:"queue_wait_mean_ms"` // media dell'attesa slot nel worker
	MaxAbsDiff    float64 `json:"max_abs_diff"`       // rispetto a expected: deve essere ~0
}

func main() {
	addrs := flag.String("addr", "unix:///tmp/sciot-w0.sock", "indirizzi dei worker, separati da virgola")
	variant := flag.String("variant", "grpc-unix", "etichetta della variante nel risultato")
	split := flag.Int("split", 0, "ultimo segmento eseguito dal device")
	inputPath := flag.String("input", "", "file input_k<split>.bin")
	expectedPath := flag.String("expected", "", "file expected_k<split>.bin")
	concurrency := flag.Int("concurrency", 1, "client in parallelo (device simulati)")
	warmup := flag.Int("warmup", 20, "richieste di riscaldamento, escluse dalla misura")
	n := flag.Int("n", 200, "richieste misurate in totale")
	flag.Parse()

	input, err := os.ReadFile(*inputPath)
	if err != nil {
		log.Fatalf("read input: %v", err)
	}
	expected, err := os.ReadFile(*expectedPath)
	if err != nil {
		log.Fatalf("read expected: %v", err)
	}

	clients, closeAll := dial(strings.Split(*addrs, ","))
	defer closeAll()

	call := func(ctx context.Context, i int) (*workerpb.RunResponse, error) {
		// Round-robin: la richiesta i va al worker i mod len(clients).
		return clients[i%len(clients)].RunFrom(ctx, &workerpb.RunRequest{
			ModelKey:   "bench",
			SplitPoint: int32(*split),
			Tensor:     input,
		})
	}

	// Riscaldamento: carica cache e connessioni, poi viene scartato.
	for i := range *warmup {
		if _, err := call(context.Background(), i); err != nil {
			log.Fatalf("warmup: %v", err)
		}
	}

	samples, elapsed, maxDiff := run(call, *concurrency, *n, expected)

	res := Result{
		Variant:     *variant,
		Workers:     len(clients),
		Split:       *split,
		Concurrency: *concurrency,
		N:           len(samples),
		InputBytes:  len(input),
		MaxAbsDiff:  maxDiff,
	}
	fillStats(&res, samples, elapsed)
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		log.Fatal(err)
	}
}

// dial apre una connessione gRPC per indirizzo. Gli indirizzi "unix:///path"
// sono socket Unix; "host:port" è TCP.
func dial(addrs []string) ([]workerpb.PartitionWorkerClient, func()) {
	var clients []workerpb.PartitionWorkerClient
	var conns []*grpc.ClientConn
	for _, addr := range addrs {
		conn, err := grpc.NewClient(addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()), // nessun TLS: stessa macchina
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(maxMessageBytes),
				grpc.MaxCallSendMsgSize(maxMessageBytes),
			),
		)
		if err != nil {
			log.Fatalf("dial %s: %v", addr, err)
		}
		conns = append(conns, conn)
		clients = append(clients, workerpb.NewPartitionWorkerClient(conn))
	}
	return clients, func() {
		for _, c := range conns {
			c.Close()
		}
	}
}

// sample è la misura di una richiesta.
type sample struct {
	latency   time.Duration // dal lato client, attesa nel worker inclusa
	queueWait float64       // ms, da RunResponse.QueueWaitMs
}

// run esegue n richieste con `concurrency` goroutine in parallelo.
// Restituisce le misure, il tempo totale e la massima differenza tra gli
// output ricevuti e expected.
func run(
	call func(context.Context, int) (*workerpb.RunResponse, error),
	concurrency, n int,
	expected []byte,
) ([]sample, time.Duration, float64) {
	var (
		next    atomic.Int64 // prossimo indice di richiesta da assegnare
		mu      sync.Mutex   // protegge samples e maxDiff
		samples = make([]sample, 0, n)
		maxDiff float64
		wg      sync.WaitGroup
	)

	start := time.Now()
	for range concurrency {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				t0 := time.Now()
				resp, err := call(context.Background(), i)
				latency := time.Since(t0)
				if err != nil {
					log.Fatalf("request %d: %v", i, err)
				}
				diff, err := maxAbsDiff(resp.GetOutput(), expected)
				if err != nil {
					log.Fatalf("request %d: %v", i, err)
				}

				mu.Lock()
				samples = append(samples, sample{latency: latency, queueWait: resp.GetQueueWaitMs()})
				maxDiff = max(maxDiff, diff)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return samples, time.Since(start), maxDiff
}

// fillStats calcola throughput, media, p50, p95 e attesa media in coda.
func fillStats(res *Result, samples []sample, elapsed time.Duration) {
	if len(samples) == 0 {
		return
	}
	latencies := make([]float64, len(samples))
	var latencySum, queueSum float64
	for i, s := range samples {
		latencies[i] = float64(s.latency) / float64(time.Millisecond)
		latencySum += latencies[i]
		queueSum += s.queueWait
	}
	sort.Float64s(latencies)

	count := float64(len(samples))
	res.ThroughputRPS = count / elapsed.Seconds()
	res.MeanMS = latencySum / count
	res.P50MS = percentile(latencies, 50)
	res.P95MS = percentile(latencies, 95)
	res.QueueWaitMS = queueSum / count
}

// percentile restituisce il q-esimo percentile (0..100) di values già ordinati,
// con lo stesso metodo di gen_inputs.py (indice arrotondato), così i numeri
// Go e Python sono confrontabili.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Round(q / 100 * float64(len(sorted)-1)))
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// maxAbsDiff interpreta a e b come float32 little-endian e restituisce la
// massima differenza assoluta. Lunghezze diverse sono un errore di protocollo.
func maxAbsDiff(a, b []byte) (float64, error) {
	if len(a) != len(b) || len(a)%4 != 0 {
		return 0, fmt.Errorf("output size %d, expected %d", len(a), len(b))
	}
	var max float64
	for i := 0; i < len(a); i += 4 {
		x := math.Float32frombits(binary.LittleEndian.Uint32(a[i:]))
		y := math.Float32frombits(binary.LittleEndian.Uint32(b[i:]))
		if d := math.Abs(float64(x) - float64(y)); d > max {
			max = d
		}
	}
	return max, nil
}
