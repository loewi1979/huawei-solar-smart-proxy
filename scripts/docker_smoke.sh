#!/bin/sh
# Isolated smoke test of an already-built image; no real inverter or GX access.
set -eu
image=${1:-huawei-smart-proxy:test}
testdir=$(mktemp -d)
container_id=
cleanup() { if [ -n "$container_id" ]; then docker rm -f "$container_id" >/dev/null; fi; }
trap cleanup EXIT
mkdir "$testdir/config" "$testdir/data"
sed -e 's/127.0.0.1:8080/0.0.0.0:8080/' \
    -e 's/token: ""/token: "test-only-smoke-password-123456"/' \
    -e 's@cache_path: cache.db@cache_path: /var/lib/huawei-smart-proxy/cache.db@' \
    config.sun2000-m1.example.yaml > "$testdir/config/config.yaml"
cp "$testdir/config/config.yaml" "$testdir/original.yaml"
chmod 700 "$testdir/config" "$testdir/data"
chmod 600 "$testdir/config/config.yaml"
sudo chown -R 10001:10001 "$testdir/config" "$testdir/data"
container_id=$(docker run -d --read-only --cap-drop ALL --security-opt no-new-privileges:true \
    -p 127.0.0.1:18080:8080 \
    -v "$testdir/config:/etc/huawei-smart-proxy" \
    -v "$testdir/data:/var/lib/huawei-smart-proxy" "$image")
base=http://127.0.0.1:18080
credentials=admin:test-only-smoke-password-123456
for attempt in $(seq 1 30); do
    if curl --silent --fail -u "$credentials" "$base/healthz" >/dev/null; then break; fi
    sleep 1
done
curl --silent --fail -u "$credentials" "$base/healthz" | grep -q ok
[ "$(curl --silent -o /dev/null -w '%{http_code}' "$base/api/config")" = 401 ]
[ "$(curl --silent -u "$credentials" -o /dev/null -w '%{http_code}' "$base/readyz")" = 503 ]
curl --silent --fail -u "$credentials" "$base/config" | grep -q 'Backup &amp; restore\|Backup & restore'
curl --silent --fail -u "$credentials" "$base/api/config" > "$testdir/download.yaml"
cmp "$testdir/original.yaml" "$testdir/download.yaml"
[ "$(curl --silent -u "$credentials" -H 'X-Proxy-Config: 1' --data-binary 'invalid_key: 1' -o /dev/null -w '%{http_code}' "$base/api/config")" = 400 ]
sed 's/log_level: info/log_level: debug/' "$testdir/original.yaml" > "$testdir/updated.yaml"
curl --silent --fail -u "$credentials" -H 'X-Proxy-Config: 1' --data-binary @"$testdir/updated.yaml" "$base/api/config"
curl --silent --fail -u "$credentials" "$base/api/config" > "$testdir/download.yaml"
cmp "$testdir/updated.yaml" "$testdir/download.yaml"
curl --silent --fail -u "$credentials" -H 'X-Proxy-Config: 1' -X POST "$base/api/config/rollback"
curl --silent --fail -u "$credentials" "$base/api/config" > "$testdir/download.yaml"
cmp "$testdir/original.yaml" "$testdir/download.yaml"
docker stop --time 90 "$container_id" >/dev/null
docker logs "$container_id"
[ "$(docker inspect --format '{{.State.ExitCode}}' "$container_id")" = 0 ]
echo 'PASS: non-root container, offline startup, authentication, stale readiness, backup/restore/rollback, graceful shutdown'
