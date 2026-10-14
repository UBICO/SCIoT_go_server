#!/usr/bin/env bash
# Esegue tutti gli scenari dello spike 2.2 e scrive out/results.jsonl.
#
#   MODEL_DIR=/path/to/yolo-small ./run_spike.sh
#
# Variabili opzionali: N (richieste per misura, default 200), THREADS (thread
# TFLite per interprete negli scenari 1-2, default 2), PY (comando python).
set -euo pipefail

cd "$(dirname "$0")"
: "${MODEL_DIR:?impostare MODEL_DIR alla cartella con i submodel_*.tflite}"
N="${N:-200}"
THREADS="${THREADS:-2}"
PY="${PY:-uv run -q --no-project --python 3.12 --with tensorflow==2.20.0 --with grpcio --with numpy python}"
OUT=out
RESULTS="$OUT/results.jsonl"
mkdir -p "$OUT"
: > "$RESULTS"

go build -o "$OUT/client" ./client

PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  PIDS=()
}
trap cleanup EXIT

# start_workers <count> <slots> <threads> <transport>: avvia i worker e
# imposta ADDRS (lista per -addr). Aspetta la riga READY di ognuno.
start_workers() {
  local count=$1 slots=$2 threads=$3 transport=$4
  ADDRS=""
  for i in $(seq 0 $((count - 1))); do
    local listen addr
    if [ "$transport" = tcp ]; then
      listen="127.0.0.1:$((50051 + i))"; addr="$listen"
    else
      listen="unix:/tmp/sciot-spike-w$i.sock"; addr="unix:///tmp/sciot-spike-w$i.sock"
      rm -f "/tmp/sciot-spike-w$i.sock"
    fi
    local log="$OUT/worker$i.log"
    $PY worker/worker.py --model-dir "$MODEL_DIR" --listen "$listen" \
      --slots "$slots" --threads "$threads" > "$log" 2>&1 &
    PIDS+=($!)
    for _ in $(seq 1 120); do
      grep -q '^READY' "$log" && break
      sleep 0.5
    done
    grep -q '^READY' "$log" || { echo "worker $i non pronto:"; cat "$log"; exit 1; }
    ADDRS="${ADDRS:+$ADDRS,}$addr"
  done
}

# rss_mb: memoria residente totale dei worker attivi, in MB.
rss_mb() {
  local kb=0
  for pid in "${PIDS[@]}"; do
    # `uv run` avvia python come processo figlio: si somma tutto l'albero.
    for p in $pid $(pgrep -P "$pid" || true); do
      kb=$((kb + $(ps -o rss= -p "$p" 2>/dev/null || echo 0)))
    done
  done
  echo $((kb / 1024))
}

# measure <label> <split> <concurrency> <extra JSON fields>
measure() {
  local label=$1 split=$2 conc=$3 extra=$4
  local line
  line=$("$OUT/client" -addr "$ADDRS" -variant "$label" -split "$split" \
    -input "$OUT/input_k$split.bin" -expected "$OUT/expected_k$split.bin" \
    -concurrency "$conc" -n "$N")
  # Aggiunge alla riga JSON i parametri dello scenario e la memoria.
  echo "${line%\}},$extra,\"rss_mb\":$(rss_mb)}" | tee -a "$RESULTS"
}

echo "== input e riferimento in-process (variante 0)"
for k in 0 4; do
  $PY worker/gen_inputs.py --model-dir "$MODEL_DIR" --split "$k" --out "$OUT" \
    --threads "$THREADS" --n "$N" | tee -a "$RESULTS"
done

echo "== scenario 1: una richiesta alla volta, unix vs tcp"
for transport in unix tcp; do
  start_workers 1 1 "$THREADS" "$transport"
  for k in 0 4; do
    measure "grpc-$transport" "$k" 1 "\"scenario\":1,\"layout\":\"threads\",\"slots\":1,\"threads\":$THREADS"
  done
  cleanup
done

echo "== scenario 2: concorrenza, pool di thread vs pool di processi (split 0)"
for slots in 1 2 4; do
  start_workers 1 "$slots" "$THREADS" unix
  for conc in 1 2 4 8; do
    measure grpc-unix 0 "$conc" "\"scenario\":2,\"layout\":\"threads\",\"slots\":$slots,\"threads\":$THREADS"
  done
  cleanup
done
for procs in 2 4; do
  start_workers "$procs" 1 "$THREADS" unix
  for conc in 1 2 4 8; do
    measure grpc-unix 0 "$conc" "\"scenario\":2,\"layout\":\"processes\",\"slots\":$procs,\"threads\":$THREADS"
  done
  cleanup
done

echo "== scenario 3: thread TFLite per interprete (2 slot, 4 client)"
for threads in 1 2 4; do
  start_workers 1 2 "$threads" unix
  measure grpc-unix 0 4 "\"scenario\":3,\"layout\":\"threads\",\"slots\":2,\"threads\":$threads"
  cleanup
done

echo "== risultati in $RESULTS"
