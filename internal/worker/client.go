// Package worker collega il server Go al worker Python (worker/):
//
//   - Client: le chiamate gRPC Describe e RunFrom;
//   - BuildCatalog: le dimensioni dei tensori per lo stream, verificate
//     contro il registry;
//   - Supervisor: avvia il processo del worker, aspetta che sia pronto, lo
//     riavvia se muore, lo ferma allo spegnimento.
package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/UBICO/SCIoT_go_server/internal/stream"
	"github.com/UBICO/SCIoT_go_server/internal/workerpb"
)

// MaxMessageBytes è il limite dei messaggi gRPC, uguale a MAX_MESSAGE_BYTES
// del worker (il default di gRPC, 4 MiB, è troppo vicino ai tensori reali).
const MaxMessageBytes = 64 << 20

// ErrInvalidJob indica un job che il worker ha rifiutato come non valido
// (modello sconosciuto, split point o dimensione errati). Lo stream valida i
// frame prima di inviarli, quindi qui significa un disallineamento tra
// catalogo e worker: è un errore di programmazione, non di carico.
var ErrInvalidJob = errors.New("job rejected by the worker")

// Client parla con un worker su un indirizzo host:port.
type Client struct {
	conn *grpc.ClientConn
	rpc  workerpb.PartitionWorkerClient
}

// Dial crea un Client. Non apre ancora la connessione: grpc la apre alla
// prima chiamata e la ristabilisce da solo se cade.
//
// Riconnessione: dopo un errore grpc aspetta prima di riprovare, con attesa
// crescente fino a 2 minuti per default. Per un worker sulla stessa macchina
// che si riavvia in pochi secondi è troppo: il limite qui è 1 s.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()), // stessa macchina
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(MaxMessageBytes),
			grpc.MaxCallSendMsgSize(MaxMessageBytes),
		),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 100 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: time.Second},
			MinConnectTimeout: time.Second,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dial worker %s: %w", addr, err)
	}
	return &Client{conn: conn, rpc: workerpb.NewPartitionWorkerClient(conn)}, nil
}

// Close chiude la connessione.
func (c *Client) Close() error { return c.conn.Close() }

// Describe chiede al worker i modelli caricati.
func (c *Client) Describe(ctx context.Context) (*workerpb.DescribeResponse, error) {
	resp, err := c.rpc.Describe(ctx, &workerpb.DescribeRequest{})
	if err != nil {
		return nil, statusToError(err)
	}
	return resp, nil
}

// Execute esegue un job sul worker. Implementa stream.Executor: finché non
// c'è il dispatcher (2.4c) lo stream può usare direttamente il Client.
//
// La scadenza del job diventa la scadenza della chiamata gRPC: il worker la
// vede (time_remaining) e non aspetta uno slot oltre quel momento.
func (c *Client) Execute(ctx context.Context, job stream.Job) (stream.Outcome, error) {
	if !job.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, job.Deadline)
		defer cancel()
	}
	resp, err := c.rpc.RunFrom(ctx, &workerpb.RunRequest{
		ModelKey:   job.ModelKey,
		SplitPoint: int32(job.SplitPoint),
		Tensor:     job.Tensor,
	})
	if err != nil {
		return stream.Outcome{}, statusToError(err)
	}
	out := stream.Outcome{LayerMS: resp.GetLayerMs(), QueueWaitMS: resp.GetQueueWaitMs()}
	if job.WantRaw {
		out.Output = resp.GetOutput()
	}
	return out, nil
}

// Il controllo a tempo di compilazione: *Client soddisfa stream.Executor.
var _ stream.Executor = (*Client)(nil)

// statusToError traduce l'errore di una chiamata gRPC negli errori che lo
// stream sa gestire (stream.ErrDeadlineExceeded, stream.ErrWorkerUnavailable)
// o in ErrInvalidJob.
//
// Il messaggio originale va conservato con %w / %v: finisce nel log del server.
func statusToError(err error) error {
	// status.Code restituisce codes.Unknown per gli errori che non vengono da gRPC.
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		// La scadenza del frame è passata: in coda nel worker o in volo.
		return fmt.Errorf("%w: %v", stream.ErrDeadlineExceeded, err)
	case codes.NotFound, codes.InvalidArgument:
		return fmt.Errorf("%w: %v", ErrInvalidJob, err)
	default:
		// Unavailable (worker giù o in riavvio), Canceled, Internal, ...
		return fmt.Errorf("%w: %v", stream.ErrWorkerUnavailable, err)
	}
}
