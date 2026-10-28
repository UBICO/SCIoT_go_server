// sciot-replay rilegge una registrazione del server (docs/RECORDING.md) per
// verificare offline i modelli della Fase 3.
//
//	sciot-replay queue [-window 5s] [-min-frames 20] record.jsonl
//
// queue: per ogni finestra, attesa in coda misurata contro quella prevista da
// M/M/c (Erlang C) e G/G/c (Allen–Cunneen), con λ, S, c, ca e cs misurati
// nella stessa finestra; poi un riassunto per fascia di utilizzo ρ.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/replay"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "queue" {
		fmt.Fprintln(os.Stderr, "usage: sciot-replay queue [-window 5s] [-min-frames 20] record.jsonl")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("queue", flag.ExitOnError)
	window := fs.Duration("window", 5*time.Second, "length of a window")
	minFrames := fs.Int("min-frames", 20, "skip windows with fewer served frames")
	fs.Parse(os.Args[2:])
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "queue: one recording file")
		os.Exit(2)
	}
	if err := queue(os.Stdout, fs.Arg(0), window.Seconds(), *minFrames); err != nil {
		fmt.Fprintln(os.Stderr, "queue:", err)
		os.Exit(1)
	}
}

func queue(out io.Writer, path string, windowS float64, minFrames int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	ws, err := replay.QueueWindows(f, windowS, minFrames)
	if err != nil {
		return err
	}
	if len(ws) == 0 {
		return fmt.Errorf("no window with at least %d served frames", minFrames)
	}
	fmt.Fprintf(out, "%8s %6s %5s %8s %8s %6s %5s %5s %12s %10s %10s\n",
		"start_s", "frames", "slots", "lambda/s", "S_ms", "rho", "ca", "cs", "measured_ms", "MMc_ms", "GGc_ms")
	for _, w := range ws {
		fmt.Fprintf(out, "%8.1f %6d %5d %8.1f %8.2f %6.3f %5.2f %5.2f %12.2f %10.2f %10.2f\n",
			w.StartS, w.Frames, w.Slots, w.Lambda, w.ServiceMS, w.Rho, w.CA, w.CS, w.MeasuredMS, w.MMcMS, w.GGcMS)
	}
	fmt.Fprintf(out, "\n%-16s %8s %12s %10s %10s\n", "rho", "windows", "measured_ms", "MMc_ms", "GGc_ms")
	for _, b := range replay.Buckets(ws) {
		if b.Windows > 0 {
			fmt.Fprintf(out, "%-16s %8d %12.2f %10.2f %10.2f\n", b.Label, b.Windows, b.MeasuredMS, b.MMcMS, b.GGcMS)
		}
	}
	return nil
}
