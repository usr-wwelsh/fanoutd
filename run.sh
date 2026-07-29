#!/usr/bin/env bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if [ -f .env ]; then
  while IFS='=' read -r key val; do
    [[ "$key" =~ ^#.*$ ]] && continue
    [[ -z "$key" ]] && continue
    val="$(echo "$val" | sed 's/^["'"'"']//;s/["'"'"']$//')"
    export "$key"="$val"
  done < .env
fi

export OPENROUTER_API_KEY="${OPENROUTER_API_KEY:?OPENROUTER_API_KEY is required. Add it to .env or export it}"
export PORT="${PORT:-8080}"

go build -o fanoutd ./cmd/fanoutd
go build -o fanout ./cmd/fanout

echo "Starting fanoutd..."
echo "  Frontend: http://localhost:5173 (dev)"
echo "  Backend:  http://localhost:${PORT}"
echo "  API key:  ${OPENROUTER_API_KEY:0:10}..."

(cd frontend && bun run dev) &
FRONTEND_PID=$!

trap "kill $FRONTEND_PID 2>/dev/null; wait $FRONTEND_PID 2>/dev/null" EXIT

./fanoutd
