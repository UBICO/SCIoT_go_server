// sciot-replay rilegge una registrazione del server (docs/RECORDING.md) per
// verificare offline i modelli e gli algoritmi della Fase 3.
//
//	sciot-replay queue [-window 5s] [-min-frames 20] record.jsonl
//	sciot-replay algo [-algorithm static|latency] [-config config.json] [-out decisions.jsonl] record.jsonl
//
// queue: per ogni finestra, attesa in coda misurata contro quella prevista da
// M/M/c (Erlang C) e G/G/c (Allen–Cunneen), con λ, S, c, ca e cs misurati
// nella stessa finestra; poi un riassunto per fascia di utilizzo ρ.
//
// algo: riesegue un algoritmo di offloading su ogni heartbeat registrato
// (con lo stato che il server aveva in quell'istante) e riassume per device
// decisioni, cambi di policy e latenze previste e misurate. A ciclo aperto:
// le decisioni non cambiano i dati che seguono.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/config"
	"github.com/UBICO/SCIoT_go_server/internal/offload"
	"github.com/UBICO/SCIoT_go_server/internal/replay"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "queue":
		fs := flag.NewFlagSet("queue", flag.ExitOnError)
		window := fs.Duration("window", 5*time.Second, "length of a window")
		minFrames := fs.Int("min-frames", 20, "skip windows with fewer served frames")
		fs.Parse(os.Args[2:])
		err = queue(os.Stdout, oneFile(fs), window.Seconds(), *minFrames)
	case "algo":
		fs := flag.NewFlagSet("algo", flag.ExitOnError)
		name := fs.String("algorithm", "static", "algorithm: "+strings.Join(offload.Names(), ", "))
		cfgPath := fs.String("config", "", "config.json whose \"offload\" section sets the algorithm's parameters (default: the defaults)")
		out := fs.String("out", "", "also write every decision as a JSON line (decision events)")
		fs.Parse(os.Args[2:])
		cfg := offload.DefaultLatencyConfig()
		if *cfgPath != "" {
			f, err := config.Load(*cfgPath)
			if err == nil {
				_, cfg, err = f.OffloadConfig()
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "algo: %v\n", err)
				os.Exit(2)
			}
		}
		alg, newErr := offload.New(*name, cfg)
		if newErr != nil {
			fmt.Fprintf(os.Stderr, "algo: %v\n", newErr)
			os.Exit(2)
		}
		err = algo(os.Stdout, oneFile(fs), alg, *out)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n  sciot-replay queue [-window 5s] [-min-frames 20] record.jsonl\n  sciot-replay algo [-algorithm static|latency] [-config config.json] [-out decisions.jsonl] record.jsonl")
	os.Exit(2)
}

func oneFile(fs *flag.FlagSet) string {
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "%s: one recording file\n", fs.Name())
		os.Exit(2)
	}
	return fs.Arg(0)
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

func algo(out io.Writer, path string, alg offload.Algorithm, decisionsPath string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	steps, err := replay.Algo(f, alg)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return fmt.Errorf("no heartbeat with the algorithm's state (recordings from phase 3.5 on)")
	}
	if decisionsPath != "" {
		if err := writeDecisions(decisionsPath, steps, alg.Name()); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "algorithm %s, open loop (decisions do not change the data that follow)\n\n", alg.Name())
	fmt.Fprintf(out, "%-16s %10s %8s %8s %22s %22s %22s\n",
		"device", "heartbeats", "differ", "changes", "predicted_actual_ms", "predicted_chosen_ms", "measured_actual_ms")
	for _, s := range replay.Summarize(steps) {
		fmt.Fprintf(out, "%-16s %10d %8d %8d %22s %22s %22s\n", s.DeviceID, s.Heartbeats, s.Differ, s.Changes,
			mean(s.PredictedActualMS, s.NPredActual), mean(s.PredictedChosenMS, s.NPredChosen), mean(s.MeasuredActualMS, s.NMeasured))
	}
	return nil
}

// mean stampa una media con quanti heartbeat la compongono.
func mean(v float64, n int) string {
	if n == 0 || math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%.2f (%d)", v, n)
}

func writeDecisions(path string, steps []replay.Step, name string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, s := range steps {
		if err := enc.Encode(replay.DecisionEvent(s, name)); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}
