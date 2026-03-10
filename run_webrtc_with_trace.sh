#!/bin/bash
# Run pion/webrtc over polycorn with leoshell trace emulation (2-path multipath)
# Usage: ./run_webrtc_with_trace.sh <trace_name> [duration_sec]

TRACE_NAME=${1:?"Usage: $0 <trace_name> [duration_sec]"}
DURATION=${2:-50}

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
POLYCORN="$SCRIPT_DIR/polycorn/target/release/polycorn_driver"
PION_DIR="$SCRIPT_DIR/pion"
LOG_DIR="$SCRIPT_DIR/logs/webrtc-trace/${TRACE_NAME}"

SERVER_ADDR_0="100.64.0.2:5001"
SERVER_ADDR_1="100.64.0.4:5003"
PION_SIGNAL_ADDR="100.64.0.2:8080"
POLYCORN_CLIENT_LISTEN="127.0.0.1:9000"
PION_DST_IP="127.0.0.2"
PION_DST_PORT="5004"

VIDEO_FILE="$PION_DIR/media/bbb_45s.h264"
VIDEO_FPS="30"

PID_LEOSHELL=""
PID_POLYCORN_CLIENT=""

export PATH="$PATH:/usr/local/go/bin"

# -- Resolve runtime library path -----------------------------------------------
WRAPPER_SO=$(find "$SCRIPT_DIR/polycorn/target/release/build" \
    -path "*/quiche-rs-*/out/libquiche_wrapper.so" | sort | tail -n 1)

if [ -z "$WRAPPER_SO" ]; then
    echo "Error: release libquiche_wrapper.so not found"
    exit 1
fi

WRAPPER_DIR="$(dirname "$WRAPPER_SO")"
LD_PATHS="$WRAPPER_DIR:$SCRIPT_DIR/polycorn/target/release:$SCRIPT_DIR/polycorn/target/release/deps${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

# -- Validate binaries -----------------------------------------------------------
if [ ! -x "$POLYCORN" ]; then
    echo "Error: polycorn driver not found or not executable: $POLYCORN"
    exit 1
fi

if [ ! -x "$PION_DIR/pion_app" ]; then
    echo "Error: pion_app not found or not executable: $PION_DIR/pion_app"
    exit 1
fi

# -- Validate trace files --------------------------------------------------------
if [ ! -f "$SCRIPT_DIR/traces/${TRACE_NAME}.up" ] || \
   [ ! -f "$SCRIPT_DIR/traces/${TRACE_NAME}.down" ]; then
    echo "Error: trace not found: traces/${TRACE_NAME}.{up,down}"
    echo "Available traces:"
    ls "$SCRIPT_DIR/traces/"*.up 2>/dev/null | sed 's/.*\///;s/\.up//'
    exit 1
fi

mkdir -p "$LOG_DIR"
[ -f "$SCRIPT_DIR/traces/delay.txt" ] || echo "50" > "$SCRIPT_DIR/traces/delay.txt"

# -- Runtime self-check ----------------------------------------------------------
echo "Checking polycorn runtime library path..."
echo "  WRAPPER_DIR=$WRAPPER_DIR"
if ! LD_LIBRARY_PATH="$LD_PATHS" "$POLYCORN" --help >/dev/null 2>&1; then
    echo "Error: polycorn_driver cannot start with LD_PATHS=$LD_PATHS"
    exit 1
fi
echo "Runtime check passed"

# -- Generate leoshell config ----------------------------------------------------
mkdir -p "$SCRIPT_DIR/config"
cat > "$SCRIPT_DIR/config/trace_config.json" <<EOF
{
    "if_num": 2,
    "log_file": "${LOG_DIR}/leoshell.log",
    "if_configs": [
        {
            "delay_interval": 5,
            "delay_filename": "${SCRIPT_DIR}/traces/delay.txt",
            "uplink": "${SCRIPT_DIR}/traces/${TRACE_NAME}.up",
            "downlink": "${SCRIPT_DIR}/traces/${TRACE_NAME}.down",
            "loss_rate": 0.00,
            "queue_params": {
                "qdisc": {"type": "codel",    "packets": 500, "target": 5, "interval": 100},
                "nic":   {"type": "droptail", "packets": 500}
            }
        },
        {
            "delay_interval": 5,
            "delay_filename": "${SCRIPT_DIR}/traces/delay.txt",
            "uplink": "${SCRIPT_DIR}/traces/${TRACE_NAME}.up",
            "downlink": "${SCRIPT_DIR}/traces/${TRACE_NAME}.down",
            "loss_rate": 0.00,
            "queue_params": {
                "qdisc": {"type": "droptail", "packets": 500},
                "nic":   {"type": "infinite"}
            }
        }
    ]
}
EOF

# -- Cleanup ---------------------------------------------------------------------
cleanup() {
    echo ""
    echo "Cleaning up..."

    if [ -n "$PID_POLYCORN_CLIENT" ]; then
        sudo kill "$PID_POLYCORN_CLIENT" 2>/dev/null || true
    fi

    if [ -n "$PID_LEOSHELL" ]; then
        kill "$PID_LEOSHELL" 2>/dev/null || true
    fi

    sudo pkill -9 polycorn_driver 2>/dev/null || true
    pkill -f "pion_app" 2>/dev/null || true

    sudo iptables -t nat -D OUTPUT -p tcp \
        -d "$PION_DST_IP" --dport "$PION_DST_PORT" \
        -j REDIRECT --to-ports 9000 2>/dev/null || true

    sudo ip addr del "${PION_DST_IP}/8" dev lo 2>/dev/null || true
    echo "Done."
}
trap cleanup EXIT

# -- Network setup ---------------------------------------------------------------
echo ""
echo "Setting up network..."
echo "----------------------------------------"

sudo ip addr add "${PION_DST_IP}/8" dev lo 2>/dev/null && \
    echo "Loopback alias $PION_DST_IP added" || \
    echo "Loopback alias $PION_DST_IP already exists, skipping"

sudo iptables -t nat -A OUTPUT -p tcp \
    -d "$PION_DST_IP" --dport "$PION_DST_PORT" \
    -j REDIRECT --to-ports 9000 && \
    echo "iptables redirect rule added" || \
    { echo "Failed to add iptables rule"; exit 1; }

# -- Start leoshell (pion server + polycorn server inside) ----------------------
echo ""
echo "Starting leoshell..."
echo "----------------------------------------"

leoshell "$SCRIPT_DIR/config/trace_config.json" /usr/bin/bash -lc "
    echo 'POLYCORN=$POLYCORN' > '$LOG_DIR/runtime_env.log'
    echo 'WRAPPER_DIR=$WRAPPER_DIR' >> '$LOG_DIR/runtime_env.log'
    echo 'LD_PATHS=$LD_PATHS' >> '$LOG_DIR/runtime_env.log'
    echo 'PION_SIGNAL_ADDR=$PION_SIGNAL_ADDR' >> '$LOG_DIR/runtime_env.log'
    echo '' >> '$LOG_DIR/runtime_env.log'
    echo '[ldd polycorn_driver]' >> '$LOG_DIR/runtime_env.log'
    env LD_LIBRARY_PATH='$LD_PATHS' ldd '$POLYCORN' >> '$LOG_DIR/runtime_env.log' 2>&1 || true

    env LD_LIBRARY_PATH='$LD_PATHS' RUST_LOG=info SIGNAL_ADDR='$PION_SIGNAL_ADDR' \
        '$PION_DIR/pion_app' -role server > '$LOG_DIR/pion_server.log' 2>&1 &
    sleep 1

    exec env LD_LIBRARY_PATH='$LD_PATHS' RUST_LOG=info \
        '$POLYCORN' server '$SERVER_ADDR_0' '$SERVER_ADDR_1' \
        > '$LOG_DIR/polycorn_server.log' 2>&1
" > "$LOG_DIR/leoshell_outer.log" 2>&1 &
PID_LEOSHELL=$!
sleep 3

if ! kill -0 "$PID_LEOSHELL" 2>/dev/null; then
    echo "leoshell failed to start"
    echo "---- leoshell_outer.log ----"
    cat "$LOG_DIR/leoshell_outer.log" 2>/dev/null || true
    echo "---- runtime_env.log ----"
    cat "$LOG_DIR/runtime_env.log" 2>/dev/null || true
    echo "---- polycorn_server.log ----"
    cat "$LOG_DIR/polycorn_server.log" 2>/dev/null || true
    exit 1
fi
echo "leoshell started (pid $PID_LEOSHELL)"

# Wait for pion server signaling port
echo -n "Waiting for pion server at $PION_SIGNAL_ADDR..."
READY=0
for i in $(seq 1 30); do
    if nc -z "100.64.0.2" 8080 2>/dev/null; then
        echo " ready (${i}s)"
        READY=1
        break
    fi

    if ! kill -0 "$PID_LEOSHELL" 2>/dev/null; then
        echo " leoshell exited unexpectedly"
        echo "---- leoshell_outer.log ----"
        cat "$LOG_DIR/leoshell_outer.log" 2>/dev/null || true
        echo "---- runtime_env.log ----"
        cat "$LOG_DIR/runtime_env.log" 2>/dev/null || true
        echo "---- pion_server.log ----"
        cat "$LOG_DIR/pion_server.log" 2>/dev/null || true
        echo "---- polycorn_server.log ----"
        cat "$LOG_DIR/polycorn_server.log" 2>/dev/null || true
        exit 1
    fi

    sleep 1
    echo -n "."
done

if [ "$READY" -ne 1 ]; then
    echo " timeout waiting for pion server"
    echo "---- pion_server.log ----"
    cat "$LOG_DIR/pion_server.log" 2>/dev/null || true
    echo "---- polycorn_server.log ----"
    cat "$LOG_DIR/polycorn_server.log" 2>/dev/null || true
    exit 1
fi

# -- Start polycorn client (outside leoshell) -----------------------------------
echo ""
echo "Starting polycorn client..."
echo "----------------------------------------"

sudo env RUST_LOG=info LD_LIBRARY_PATH="$LD_PATHS" \
    "$POLYCORN" client \
    -l "$POLYCORN_CLIENT_LISTEN" \
    -i lo \
    "$SERVER_ADDR_0" "$SERVER_ADDR_1" \
    > "$LOG_DIR/polycorn_client.log" 2>&1 &
PID_POLYCORN_CLIENT=$!
sleep 2

if ! sudo kill -0 "$PID_POLYCORN_CLIENT" 2>/dev/null; then
    echo "Polycorn client failed to start"
    cat "$LOG_DIR/polycorn_client.log" 2>/dev/null || true
    exit 1
fi
echo "Polycorn client started (pid $PID_POLYCORN_CLIENT)"

# -- Run pion client (foreground) -----------------------------------------------
echo ""
echo "Starting pion client (trace=$TRACE_NAME, duration=${DURATION}s)..."
echo "=================================================="

LOG_DIR="$LOG_DIR" \
SIGNAL_ADDR="$PION_SIGNAL_ADDR" \
VIDEO_FILE="$VIDEO_FILE" \
VIDEO_FPS="$VIDEO_FPS" \
"$PION_DIR/pion_app" -role client

CLIENT_EXIT=$?

echo ""
echo "=================================================="
if [ $CLIENT_EXIT -eq 0 ]; then
    echo "Test completed successfully"
    echo "  Trace:    $TRACE_NAME"
    echo "  Duration: ${DURATION}s"
else
    echo "Test failed (exit code $CLIENT_EXIT)"
fi

echo ""
echo "Logs:"
echo "  leoshell        -> $LOG_DIR/leoshell_outer.log"
echo "  runtime env     -> $LOG_DIR/runtime_env.log"
echo "  pion server     -> $LOG_DIR/pion_server.log"
echo "  polycorn server -> $LOG_DIR/polycorn_server.log"
echo "  polycorn client -> $LOG_DIR/polycorn_client.log"
echo "  results         -> $LOG_DIR/results.json"
echo "  summary         -> $LOG_DIR/summary.txt"
