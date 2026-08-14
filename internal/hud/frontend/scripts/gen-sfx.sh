#!/usr/bin/env bash
# Regenerate the Factory loom SFX from libs/py-chiptune (flexinfer-chiptune).
#
# The three WAVs under src/lib/assets/sfx/ are checked in; run this only
# when changing the sound design, then commit the new assets. Mapping:
#   clack  ← chiptune sfx land     (reed beat laying a pick)
#   chime  ← chiptune sfx success  (bolt rolled off — merged)
#   klaxon ← chiptune sfx error    (spark — escalated)
set -euo pipefail
cd "$(dirname "$0")/../src/lib/assets/sfx"

# Override when the workspace layout differs (default: canonical checkout).
CHIPTUNE_DIR="${CHIPTUNE_DIR:-$(cd ../../../../../../.. && pwd)/libs/py-chiptune}"

gen() { (cd "$CHIPTUNE_DIR" && uv run chiptune sfx "$1" --format wav --output "$OLDPWD/$2"); }
gen land clack.wav
gen success chime.wav
gen error klaxon.wav
echo "regenerated: $(ls -la ./*.wav | awk '{print $NF, "("$5")"}' | tr '\n' ' ')"
