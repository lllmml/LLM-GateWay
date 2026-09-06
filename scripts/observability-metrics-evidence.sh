#!/bin/sh
# Week 10 A3c metrics evidence orchestration.
#
# Brings up the full local stack (postgres + observability) with the gateway
# binary running on the host, then runs the metrics-evidence integration test
# (internal/store/postgres/observability_metrics_evidence_test.go) which fires
# a REAL request and asserts the counted durable metric is visible through
# Prometheus AND the Grafana Prometheus datasource, with the auto-provisioned
# Gateway Overview dashboard importable.
#
# Environment is inherited from the Makefile recipe (which includes and
# exports .env), so DATABASE_URL / CREDENTIAL_MASTER_KEY / VIRTUAL_KEY_PEPPER
# match the gateway binary started here.
set -eu

GO="${1:-go}"
ROOT="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

GATEWAY_BIN="$(mktemp -d)/gateway-evidence"
cleanup() {
	if [ -n "${GATEWAY_PID:-}" ]; then
		kill "$GATEWAY_PID" 2>/dev/null || true
		wait "$GATEWAY_PID" 2>/dev/null || true
	fi
	rm -rf "$(dirname "$GATEWAY_BIN")"
}
trap cleanup EXIT INT TERM

docker compose up -d --wait postgres >/dev/null

make migrate-up >/dev/null
make observability-up >/dev/null
# Grafana only reads provisioning providers at startup; restart so the A3c
# dashboard provider (added this slice) is loaded before the evidence runs.
docker compose restart grafana >/dev/null
for i in $(seq 1 90); do
	if curl -fsS http://127.0.0.1:3001/api/health >/dev/null 2>&1; then
		break
	fi
	sleep 1
done

echo "building gateway binary..."
"$GO" build -o "$GATEWAY_BIN" ./cmd/gateway

echo "starting gateway (data :18080, control :18081, ops :9090)..."
DATA_PLANE_ADDR=:18080 \
CONTROL_PLANE_ADDR=:18081 \
"$GATEWAY_BIN" >/tmp/gateway-observability-evidence.log 2>&1 &
GATEWAY_PID=$!

for i in $(seq 1 60); do
	if curl -fsS http://127.0.0.1:9090/health/live >/dev/null 2>&1; then
		break
	fi
	if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
		echo "gateway exited early; log:" >&2
		tail -20 /tmp/gateway-observability-evidence.log >&2 || true
		exit 1
	fi
	sleep 1
done

echo "running metrics evidence..."
"$GO" test -tags=integration ./internal/store/postgres/ -run '^TestObservabilityMetricsEvidence$' -count=1 -v
