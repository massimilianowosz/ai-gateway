#!/usr/bin/env bash
# scripts/update_prices.sh
#
# Downloads model pricing from LiteLLM's upstream source and converts it
# to our simplified format (model_name → input/output cost per token).
#
# Usage:
#   ./scripts/update_prices.sh
#
# This updates internal/pricing/model_prices.json in-place.
# Review the diff and commit when ready.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OUTPUT="$REPO_ROOT/internal/pricing/model_prices.json"
UPSTREAM_URL="https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

echo "⬇ Downloading upstream pricing..."
TMPFILE=$(mktemp)
trap 'rm -f "$TMPFILE"' EXIT

curl -sS --fail -o "$TMPFILE" "$UPSTREAM_URL"

echo "🔄 Converting to ubiquum format..."

# Use Python (available on macOS/Linux) to transform the JSON:
# - Keep only models with input_cost_per_token or output_cost_per_token > 0
# - Keep input/output cost plus the two prompt-cache tariffs
# - Sort keys for stable diffs
python3 - "$TMPFILE" "$OUTPUT" << 'PYTHON'
import json
import sys

with open(sys.argv[1]) as f:
    raw = json.load(f)

prices = {}
for name, data in raw.items():
    if name == "sample_spec":
        continue
    if not isinstance(data, dict):
        continue
    input_cost = data.get("input_cost_per_token", 0) or 0
    output_cost = data.get("output_cost_per_token", 0) or 0
    if input_cost > 0 or output_cost > 0:
        # Use the short name (strip provider prefix for cleaner keys)
        entry = {
            "input_cost_per_token": input_cost,
            "output_cost_per_token": output_cost
        }
        # Prompt-cache tariffs. Omitted when upstream has none: the gateway
        # then falls back to the input price rather than billing zero.
        cache_read = data.get("cache_read_input_token_cost", 0) or 0
        cache_creation = data.get("cache_creation_input_token_cost", 0) or 0
        if cache_read > 0:
            entry["cache_read_input_token_cost"] = cache_read
        if cache_creation > 0:
            entry["cache_creation_input_token_cost"] = cache_creation
        prices[name] = entry

# Also create short-name aliases (e.g., "gpt-4o" from "azure/gpt-4o")
aliases = {}
for name, p in prices.items():
    if "/" in name:
        short = name.split("/", 1)[1]
        # Only add alias if not already a direct entry
        if short not in prices and short not in aliases:
            aliases[short] = p

prices.update(aliases)

with open(sys.argv[2], "w") as f:
    json.dump(prices, f, indent=2, sort_keys=True)
    f.write("\n")

print(f"✅ Written {len(prices)} model prices to {sys.argv[2]}")
PYTHON

echo ""
echo "Done! Review changes with:"
echo "  git diff internal/pricing/model_prices.json"
echo ""
echo "Then commit:"
echo "  git add internal/pricing/model_prices.json"
echo "  git commit -m 'pricing: update model prices from upstream'"
