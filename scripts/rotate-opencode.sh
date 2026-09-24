#!/usr/bin/env bash
# Rotate the OpenCode BYOK credential.
#
# The vault has no update path — Vault.Create refuses a duplicate name — so a
# rotation is a delete followed by a create. The key is read from the
# environment and never appears in an argument, so it stays out of shell
# history and out of any transcript.
#
#   export OPENCODE_KEY='oc_sk_...'
#   ./scripts/rotate-opencode.sh
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

: "${OPENCODE_KEY:?set OPENCODE_KEY first (export OPENCODE_KEY='oc_sk_...')}"
MK=$(tr -d '[:space:]' < tmp/master-key.txt)
BASE=${GATEWAY_URL:-http://127.0.0.1:4000}
NAME=${CONNECTION_NAME:-OpenCode}

api() { curl -sS -H "Authorization: Bearer $MK" -H 'Content-Type: application/json' "$@"; }

ID=$(api "$BASE/console/api/connections" | python3 -c "
import json,sys
for c in json.load(sys.stdin).get('connections', []):
    if c['name'].lower() == '$NAME'.lower():
        print(c['id']); break
")

if [ -n "$ID" ]; then
  api -X DELETE "$BASE/console/api/connections/$ID" >/dev/null
  echo "removed the old '$NAME' connection"
fi

python3 - <<PY | api -X POST "$BASE/console/api/connections" -d @- >/dev/null
import json, os

key = os.environ["OPENCODE_KEY"].strip()
# A key pasted after a "oc_sk_..." example tends to arrive with the prefix
# twice, and upstream rejects it as simply invalid — a confusing way to find
# out about a typo.
while key.startswith("oc_sk_oc_sk_"):
    key = key[len("oc_sk_"):]

print(json.dumps({
    "name": "$NAME",
    "provider_type": "openai_compatible",
    "api_base": "https://opencode.ai/inference/openai/v1",
    "api_key": key,
    "models": [
        {"name": "jev-1.13", "provider_model": "jev-1.13"},
        {"name": "jev-1.13-free", "provider_model": "jev-1.13-free"},
        {"name": "deepseek-v4.1-flash", "provider_model": "deepseek-v4.1-flash"},
        {"name": "glm-5.3-flash", "provider_model": "glm-5.3-flash"},
    ],
}))
PY
echo "recreated '$NAME' with the new credential"

echo -n "smoke test: "
KEY=$(tr -d '[:space:]' < tmp/claude-code-key.txt)
curl -s -o /tmp/rot.json -w 'HTTP %{http_code}\n' -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"jev-1.13","max_tokens":10,"messages":[{"role":"user","content":"say OK"}]}'
head -c 160 /tmp/rot.json; echo; rm -f /tmp/rot.json
