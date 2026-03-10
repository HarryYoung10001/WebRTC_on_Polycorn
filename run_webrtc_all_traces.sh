#!/bin/bash

# Run webrtc-over-polycorn tests with all available traces
# Usage: ./run_webrtc_all_trace.sh [duration_sec]

DURATION=${1:-50}
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_SCRIPT="$SCRIPT_DIR/run_webrtc_with_trace.sh"
TRACE_DIR="$SCRIPT_DIR/traces"
LOG_ROOT="$SCRIPT_DIR/logs/webrtc-trace"

if [ ! -x "$RUN_SCRIPT" ]; then
    echo "Error: run script not found or not executable: $RUN_SCRIPT"
    exit 1
fi

if [ ! -d "$TRACE_DIR" ]; then
    echo "Error: trace directory not found: $TRACE_DIR"
    exit 1
fi

echo "Running webrtc trace tests with all traces (${DURATION}s each)"
echo "=================================================="

TRACES=$(find "$TRACE_DIR" -maxdepth 1 -name "*.up" -printf "%f\n" | sed 's/\.up$//' | sort)

if [ -z "$TRACES" ]; then
    echo "No trace files found in $TRACE_DIR"
    exit 1
fi

PASS_COUNT=0
FAIL_COUNT=0

for TRACE in $TRACES; do
    echo ""
    echo "Testing trace: $TRACE"
    echo "----------------------------------------"

    bash "$RUN_SCRIPT" "$TRACE" "$DURATION"
    STATUS=$?

    RESULT_JSON="$LOG_ROOT/$TRACE/results.json"
    SUMMARY_TXT="$LOG_ROOT/$TRACE/summary.txt"

    if [ $STATUS -eq 0 ]; then
        echo "✓ $TRACE completed successfully"
        PASS_COUNT=$((PASS_COUNT + 1))
    else
        echo "✗ $TRACE failed"
        FAIL_COUNT=$((FAIL_COUNT + 1))
    fi

    if [ -f "$RESULT_JSON" ]; then
        echo "Result summary:"
        python3 - "$RESULT_JSON" <<'PY'
import json
import sys

path = sys.argv[1]
try:
    with open(path, "r") as f:
        data = json.load(f)

    throughput = data.get("throughput_mbps")
    latency = data.get("avg_latency_ms")
    tx_packets = data.get("packets_tx")
    rx_packets = data.get("packets_rx")
    tx_bytes = data.get("bytes_tx")
    rx_bytes = data.get("bytes_rx")

    parts = []
    if throughput is not None:
        parts.append(f"Throughput: {throughput:.2f} Mbps")
    if latency is not None:
        parts.append(f"Avg Latency: {latency:.2f} ms")
    if tx_packets is not None and rx_packets is not None:
        parts.append(f"Packets: {tx_packets} tx / {rx_packets} rx")
    if tx_bytes is not None and rx_bytes is not None:
        parts.append(f"Bytes: {tx_bytes} tx / {rx_bytes} rx")

    if parts:
        print("  " + " | ".join(parts))
    else:
        print("  results.json found, but no expected fields were present")
except Exception as e:
    print(f"  Failed to parse {path}: {e}")
PY
    elif [ -f "$SUMMARY_TXT" ]; then
        echo "Result summary:"
        sed 's/^/  /' "$SUMMARY_TXT"
    else
        echo "No results file found for $TRACE"
    fi

    sleep 3
done

echo ""
echo "=================================================="
echo "All tests complete!"
echo "  Passed: $PASS_COUNT"
echo "  Failed: $FAIL_COUNT"
echo "  Results root: $LOG_ROOT"

echo ""
echo "Summary of all traces:"
for TRACE in $TRACES; do
    RESULT_JSON="$LOG_ROOT/$TRACE/results.json"
    SUMMARY_TXT="$LOG_ROOT/$TRACE/summary.txt"

    echo -n "$TRACE: "

    if [ -f "$RESULT_JSON" ]; then
        python3 - "$RESULT_JSON" <<'PY'
import json
import sys

path = sys.argv[1]
try:
    with open(path, "r") as f:
        data = json.load(f)

    throughput = data.get("throughput_mbps")
    latency = data.get("avg_latency_ms")

    if throughput is not None and latency is not None:
        print(f"{throughput:.2f} Mbps, {latency:.2f} ms")
    elif throughput is not None:
        print(f"{throughput:.2f} Mbps")
    else:
        print("results.json present, but throughput field missing")
except Exception as e:
    print(f"parse failed: {e}")
PY
    elif [ -f "$SUMMARY_TXT" ]; then
        head -n 1 "$SUMMARY_TXT"
    else
        echo "no result"
    fi
done
