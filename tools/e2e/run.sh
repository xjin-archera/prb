#!/bin/sh
# Offline browser test: a fake `gh`, an isolated state dir, and headless Chrome driven over the debugging
# protocol. Needs Chrome and uv. Makes no real gh call and runs no review.
set -e
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
tmp=$(mktemp -d)
mkdir -p "$tmp/bin" "$tmp/state/logs"
cp "$here/fake-gh" "$tmp/bin/gh"; chmod +x "$tmp/bin/gh"
echo '{"repos":{"acme/app":"/tmp/does-not-matter"},"scan_dirs":[],"runner":"host","port":8791,"include_teams":false}' > "$tmp/state/config.json"
(cd "$root" && go build -o "$tmp/prb" ./cmd/prb)
PATH="$tmp/bin:$PATH" PRB_STATE_DIR="$tmp/state" "$tmp/prb" > "$tmp/server.log" 2>&1 & pid=$!
trap 'kill $pid 2>/dev/null; sleep 1; rm -rf "$tmp" 2>/dev/null' EXIT
for _ in $(seq 1 30); do curl -sf -o /dev/null http://127.0.0.1:8791/ && break; sleep 0.3; done
kill $pid; wait $pid 2>/dev/null || true
sqlite3 "$tmp/state/prb.db" "INSERT INTO reviews (key,repo,number,status,head_sha,worktree,finished_at,result,session_id,rounds) VALUES ('acme/app#7','acme/app',7,'done','abcdef1234567890','/tmp/does-not-matter',1758500000,'{\"verdict\":\"APPROVE\",\"verified_locally\":\"\",\"summary_body\":\"ok\",\"cut\":[],\"lgtm\":true}','s1',1); INSERT INTO comments (id,review_key,position,path,line,side,severity,body,include,ai_generated) VALUES ('c1','acme/app#7',0,'a.py',3,'RIGHT','Nit','**Nit:** name',1,1);"
PATH="$tmp/bin:$PATH" PRB_STATE_DIR="$tmp/state" "$tmp/prb" > "$tmp/server.log" 2>&1 & pid=$!
for _ in $(seq 1 30); do curl -sf -o /dev/null http://127.0.0.1:8791/ && break; sleep 0.3; done
cd "$here" && uv run --quiet --with websocket-client python drive.py "http://127.0.0.1:8791/" "$tmp/chrome" "${1:-$tmp/e2e.png}"
