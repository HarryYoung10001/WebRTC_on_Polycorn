#!/bin/bash
# Run pion/webrtc over polycorn multipath transport (single-machine test)

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
POLYCORN="$SCRIPT_DIR/polycorn/target/release/polycorn_driver"
PION_DIR="$SCRIPT_DIR/pion"
QUICHE_LIB="$HOME/projects/SatHSR/polycorn/target/release/build/quiche-rs-5651dff367648393/out"
LOG_DIR="$SCRIPT_DIR/logs/webrtc"

POLYCORN_SERVER_ADDR="127.0.0.1:5001"
POLYCORN_CLIENT_LISTEN="127.0.0.1:9000"
PION_SERVER_IP="127.0.0.2"
PION_SERVER_PORT="5004"

export PATH=$PATH:/usr/local/go/bin

echo "Running pion/webrtc over polycorn"
echo "=================================================="

# Cleanup on exit
cleanup() {
    echo ""
    echo "Cleaning up..."
    sudo kill "$PID_POLYCORN_SERVER" "$PID_POLYCORN_CLIENT" \
         2>/dev/null || true
    kill "$PID_PION_SERVER" 2>/dev/null || true
    sudo pkill -9 polycorn_driver 2>/dev/null || true
    pkill -f "pion_app" 2>/dev/null || true
    pkill -f "go run . -role server" 2>/dev/null || true   
    sudo fuser -k 5004/tcp 2>/dev/null || true             
    sudo iptables -t nat -D OUTPUT -p tcp \
        -d "$PION_SERVER_IP" --dport "$PION_SERVER_PORT" \
        -j REDIRECT --to-ports 9000 2>/dev/null || true
    sudo ip addr del "${PION_SERVER_IP}/8" dev lo 2>/dev/null || true
    echo "Done."
}
trap cleanup EXIT

# Network setup
echo ""
echo "Setting up network..."
echo "----------------------------------------"

sudo ip addr add "${PION_SERVER_IP}/8" dev lo 2>/dev/null && \
    echo "✓ Loopback alias $PION_SERVER_IP added" || \
    echo "- Loopback alias $PION_SERVER_IP already exists, skipping"

sudo iptables -t nat -A OUTPUT -p tcp \
    -d "$PION_SERVER_IP" --dport "$PION_SERVER_PORT" \
    -j REDIRECT --to-ports 9000 && \
    echo "✓ iptables rule added" || \
    { echo "✗ Failed to add iptables rule"; exit 1; }

# Create log directory
echo ""
echo "Creating log directory..."
echo "----------------------------------------"
mkdir -p "$LOG_DIR" && \
    echo "✓ Log directory created: $LOG_DIR" || \
    { echo "✗ Failed to create log directory"; exit 1; }

# Start polycorn server
echo ""
echo "Starting polycorn server..."
echo "----------------------------------------"
sudo RUST_LOG=info LD_LIBRARY_PATH=$LD_LIBRARY_PATH:$QUICHE_LIB \
    "$POLYCORN" server "$POLYCORN_SERVER_ADDR" \
    > "$LOG_DIR/polycorn_server.log" 2>&1 &
PID_POLYCORN_SERVER=$!
sleep 1

if sudo kill -0 "$PID_POLYCORN_SERVER" 2>/dev/null; then
    echo "✓ Polycorn server started (pid $PID_POLYCORN_SERVER)"
else
    echo "✗ Polycorn server failed to start"
    cat "$LOG_DIR/polycorn_server.log"
    exit 1
fi

# Start polycorn client
echo ""
echo "Starting polycorn client..."
echo "----------------------------------------"
sudo RUST_LOG=info LD_LIBRARY_PATH=$LD_LIBRARY_PATH:$QUICHE_LIB \
    "$POLYCORN" client \
    -l "$POLYCORN_CLIENT_LISTEN" \
    -i lo \
    "$POLYCORN_SERVER_ADDR" \
    > "$LOG_DIR/polycorn_client.log" 2>&1 &
PID_POLYCORN_CLIENT=$!
sleep 1

if sudo kill -0 "$PID_POLYCORN_CLIENT" 2>/dev/null; then
    echo "✓ Polycorn client started (pid $PID_POLYCORN_CLIENT)"
else
    echo "✗ Polycorn client failed to start"
    cat "$LOG_DIR/polycorn_client.log"
    exit 1
fi

# Start pion server
echo ""
echo "Starting pion server..."
echo "----------------------------------------"
"$PION_DIR/pion_app" -role server > "$LOG_DIR/pion_server.log" 2>&1 &
PID_PION_SERVER=$!

# Wait for signaling port to be ready (up to 30s)
echo -n "Waiting for pion server..."
for i in $(seq 1 30); do
    if nc -z 127.0.0.1 8080 2>/dev/null; then
        echo " ready (${i}s)"
        break
    fi
    if ! kill -0 "$PID_PION_SERVER" 2>/dev/null; then
        echo " crashed"
        echo "✗ Pion server failed to start"
        cat "$LOG_DIR/pion_server.log"
        exit 1
    fi
    sleep 1
    echo -n "."
done


if ! nc -z 127.0.0.1 8080 2>/dev/null; then
    echo ""
    echo "✗ Pion server did not start within 60s"
    cat "$LOG_DIR/pion_server.log"
    exit 1
fi
echo "✓ Pion server started (pid $PID_PION_SERVER)"

# Run pion client (foreground)
echo ""
echo "Starting pion client..."
echo "=================================================="
LOG_DIR="$LOG_DIR" "$PION_DIR/pion_app" -role client
CLIENT_EXIT=$?

echo ""
echo "=================================================="
if [ $CLIENT_EXIT -eq 0 ]; then
    echo "✓ Test completed successfully"
else
    echo "✗ Test failed (exit code $CLIENT_EXIT)"
fi

echo ""
echo "Logs:"
echo "  polycorn server -> $LOG_DIR/polycorn_server.log"
echo "  polycorn client -> $LOG_DIR/polycorn_client.log"
echo "  pion server     -> $LOG_DIR/pion_server.log"
echo "  results         -> $LOG_DIR/results.json"
echo "  summary         -> $LOG_DIR/summary.txt"