#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$ROOT"

case "$(uname -m)" in
    aarch64|arm64) ;;
    *)
        echo "This script requires Linux ARM64/aarch64; detected: $(uname -m)" >&2
        exit 1
        ;;
esac

SERVER="runtime/bin/qqt-server-linux-arm64"
CONFIG="configs/server-directory-local-ui-linux-arm64.json"
ONNX_RUNTIME_DIR="runtime/onnxruntime/linux-arm64"
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

echo "QQTang headless server is starting (Linux ARM64)."
echo "Press Ctrl+C to stop it. Network settings: configs/network.json"
exec "$SERVER" -config "$CONFIG" "$@"
