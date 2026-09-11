#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$ROOT"

case "$(uname -m)" in
    x86_64|amd64) ;;
    *)
        echo "This script requires Linux AMD64/x86_64; detected: $(uname -m)" >&2
        exit 1
        ;;
esac

SERVER="runtime/bin/qqt-server-linux-amd64"
CONFIG="configs/server-directory-local-ui-linux-amd64.json"
ONNX_RUNTIME_DIR="runtime/onnxruntime/linux-amd64"
if [ ! -f "$SERVER" ]; then
    echo "Missing server executable: $SERVER" >&2
    echo "Please extract the complete QQTang-Local release package." >&2
    exit 1
fi
if [ ! -f "$CONFIG" ]; then
    echo "Missing server configuration: $CONFIG" >&2
    echo "Please extract the complete QQTang-Local release package." >&2
    exit 1
fi


mkdir -p runtime/data runtime/logs runtime/captures
chmod +x "$SERVER"
export LD_LIBRARY_PATH="$ROOT/$ONNX_RUNTIME_DIR${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

echo "QQTang headless server is starting (Linux AMD64)."
echo "Press Ctrl+C to stop it. Network settings: configs/network.json"
exec "$SERVER" -config "$CONFIG" "$@"
