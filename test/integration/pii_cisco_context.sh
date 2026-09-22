#!/usr/bin/env bash
# Simulates the customer flow that loads a Cisco-like text file into chat
# context, then prints both the model result and the observed PII behaviour.
#
# Usage:
#   UBIQUUM_API_KEY=sk-ubq-... ./test/integration/pii_cisco_context.sh
#
# Optional:
#   UBIQUUM_BASE_URL=https://api.ubiquum.ai
#   UBIQUUM_MODEL=gpt-4o
#   CISCO_CONTEXT_FILE=/path/to/router.txt
#   PII_EXPECT=auto|redact|block|off
#   SHOW_RAW_RESPONSE=1
set -Eeuo pipefail

base_url="${UBIQUUM_BASE_URL:-https://api.ubiquum.ai}"
model="${UBIQUUM_MODEL:-gpt-4o}"
expected="${PII_EXPECT:-auto}"
context_file="${CISCO_CONTEXT_FILE:-}"

: "${UBIQUUM_API_KEY:?Set UBIQUUM_API_KEY to a tenant-scoped Ubiquum key}"

for command_name in curl python3; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    printf 'Missing required command: %s\n' "$command_name" >&2
    exit 1
  fi
done

case "$expected" in
  auto|redact|block|off) ;;
  *)
    printf 'PII_EXPECT must be one of: auto, redact, block, off\n' >&2
    exit 1
    ;;
esac

if [[ -n "$context_file" ]]; then
  if [[ ! -r "$context_file" ]]; then
    printf 'CISCO_CONTEXT_FILE is not readable: %s\n' "$context_file" >&2
    exit 1
  fi
  cisco_context="$(<"$context_file")"
else
  read -r -d '' cisco_context <<'CISCO' || true
!! IOS XR configuration 7.9.2 - synthetic test fixture
hostname BETA80-EDGE-XR-01
!
ipv4 access-list EDGE-IN
 10 permit ipv4 host 198.51.100.44 host 10.80.0.15
 20 permit tcp 203.0.113.0 0.0.0.255 any eq 443
 90 deny ipv4 any any log
!
interface GigabitEthernet0/0/0/0
 description WAN_TO_MPLS
 ipv4 address 192.0.2.10 255.255.255.252
 ipv6 address 2001:db8:80::2/64
 no shutdown
!
router bgp 65080
 bgp router-id 10.80.0.1
 neighbor 192.0.2.9
  remote-as 64520
  address-family ipv4 unicast
 neighbor 2001:db8:80::1
  remote-as 64520
  address-family ipv6 unicast
!
route-policy ACCEPT-BETA80
 if destination in (203.0.113.0/24) then
  pass
 else
  drop
 endif
end-policy
CISCO
fi

prompt="$(printf '%s\n\n%s' \
  'Il testo seguente è il contenuto sintetico di un file Cisco caricato nel contesto. Restituisci esattamente, senza commenti e senza Markdown, tutte le righe che iniziano con ipv4, ipv6, neighbor, permit o deny. Non correggere e non omettere i valori.' \
  "$cisco_context")"

payload="$(printf '%s' "$prompt" | python3 -c 'import json,sys; print(json.dumps({"model":sys.argv[1],"temperature":0,"max_tokens":700,"messages":[{"role":"user","content":sys.stdin.read()}]},separators=(",",":")))' "$model")"

printf '\n=== Ubiquum Cisco-context PII test ===\n'
printf 'Endpoint: %s/v1/chat/completions\n' "$base_url"
printf 'Model:    %s\n' "$model"
printf 'Expected: %s\n' "$expected"
if [[ -n "$context_file" ]]; then
  printf 'Source:   %s\n' "$context_file"
else
  printf 'Source:   built-in synthetic Cisco fixture\n'
fi
printf '\n--- Content added to chat context ---\n%s\n' "$cisco_context"

if ! response="$(curl -sS --max-time 120 -w '\n%{http_code}' \
  -X POST "$base_url/v1/chat/completions" \
  -H "Authorization: Bearer $UBIQUUM_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'x-ubiquum-cache: false' \
  --data "$payload")"; then
  printf '\nRequest failed before receiving an HTTP response.\n' >&2
  exit 1
fi

http_status="${response##*$'\n'}"
body="${response%$'\n'*}"
printf '\n--- HTTP status ---\n%s\n' "$http_status"

if [[ "${SHOW_RAW_RESPONSE:-0}" == "1" ]]; then
  printf '\n--- Raw API response ---\n'
  printf '%s' "$body" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$body"
fi

observed="unknown"
if [[ "$http_status" == "403" ]]; then
  observed="block"
  error_message="$(printf '%s' "$body" | python3 -c 'import json,sys; data=json.load(sys.stdin); print(data.get("error",{}).get("message",json.dumps(data)))' 2>/dev/null || printf '%s' "$body")"
  printf '\n--- Guardrail result ---\nBLOCKED\n%s\n' "$error_message"
elif [[ "$http_status" == "200" ]]; then
  assistant_content="$(printf '%s' "$body" | python3 -c 'import json,sys; data=json.load(sys.stdin); print(data["choices"][0]["message"]["content"])')"
  printf '\n--- Model result ---\n%s\n' "$assistant_content"

  analysis="$(SOURCE_TEXT="$cisco_context" RESULT_TEXT="$assistant_content" python3 -c '
import ipaddress
import os
import re

source = os.environ["SOURCE_TEXT"]
result = os.environ["RESULT_TEXT"]
candidate_pattern = re.compile(r"(?<![0-9A-Fa-f:.])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9.])|(?<![0-9A-Fa-f:])(?:[0-9A-Fa-f]{0,4}:){2,7}[0-9A-Fa-f]{0,4}(?![0-9A-Fa-f:])")

def valid_addresses(text):
    addresses = set()
    for match in candidate_pattern.finditer(text):
        value = match.group(0)
        try:
            addresses.add(str(ipaddress.ip_address(value)))
        except ValueError:
            pass
    return addresses

raw_hits = sorted(valid_addresses(source) & valid_addresses(result))
print(result.count("[IP_ADDRESS]"))
print(", ".join(raw_hits))
')"
  placeholder_count="$(printf '%s\n' "$analysis" | sed -n '1p')"
  raw_hits="$(printf '%s\n' "$analysis" | sed -n '2p')"

  if [[ "$placeholder_count" -gt 0 && -z "$raw_hits" ]]; then
    observed="redact"
    printf '\n--- Guardrail result ---\nREDACTED: found %s [IP_ADDRESS] placeholder(s); no source IP reached the model output.\n' "$placeholder_count"
  elif [[ -n "$raw_hits" ]]; then
    observed="off"
    printf '\n--- Guardrail result ---\nNOT REDACTED: source IP visible in model output: %s\n' "$raw_hits"
  else
    printf '\n--- Guardrail result ---\nINCONCLUSIVE: request passed, but the model output contains neither source IPs nor [IP_ADDRESS].\n'
  fi
else
  printf '\n--- API error ---\n'
  printf '%s' "$body" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$body"
fi

if [[ "$expected" != "auto" && "$observed" != "$expected" ]]; then
  printf '\nFAIL: expected %s, observed %s (HTTP %s).\n' "$expected" "$observed" "$http_status" >&2
  exit 1
fi

if [[ "$observed" == "unknown" ]]; then
  printf '\nFAIL: unexpected HTTP status %s.\n' "$http_status" >&2
  exit 1
fi

printf '\nPASS: observed PII mode = %s\n' "$observed"
