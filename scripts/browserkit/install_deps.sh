#!/usr/bin/env bash
set -euo pipefail

PY_BASE="${BROWSERKIT_PYTHON_BASE:-python3}"
VENV_DIR="${BROWSERKIT_VENV_DIR:-$HOME/.config/loom/browserkit-venv}"

if ! command -v "$PY_BASE" >/dev/null 2>&1; then
  echo "ERROR: '$PY_BASE' not found. Set BROWSERKIT_PYTHON_BASE or install python3." >&2
  exit 1
fi

echo "Setting up BrowserKit venv at: $VENV_DIR"
if [ ! -x "$VENV_DIR/bin/python" ] && [ ! -x "$VENV_DIR/bin/python3" ]; then
  set -x
  "$PY_BASE" -m venv "$VENV_DIR"
  set +x
fi

PY="$VENV_DIR/bin/python"
if [ ! -x "$PY" ]; then
  PY="$VENV_DIR/bin/python3"
fi
if [ ! -x "$PY" ]; then
  echo "ERROR: venv python not found under $VENV_DIR/bin" >&2
  exit 2
fi

echo "Using Python: $("$PY" --version 2>/dev/null || true)"

# Build backend bootstrap only. Deliberately left floating: pip/setuptools/wheel
# are not shipped dependencies of anything in this repo, and an old pip cannot
# resolve the PEP 508 direct reference below.
echo "Upgrading pip tooling..."
set -x
"$PY" -m pip install -U pip setuptools wheel
set +x

# --- BrowserKit dependency pins ----------------------------------------------
# flexinfer-browser-kit is an internal library (libs/py-browser-kit), so it is
# pinned to a `main` commit SHA through a direct git reference, per the
# "Internal Dependency Pinning" rule in libs/STANDARDS.md.
#
# This used to be `pip install -U flexinfer-browser-kit`, which resolved
# whatever the public PyPI index happened to be serving. Two MCP servers import
# the package at runtime -- cmd/mcp-browserkit (screenshot_helper.py) and
# cmd/mcp-linkedin (browserkit_helper.py), both doing
# `from browser_kit.browser import BrowserConfig, BrowserManager` -- so an
# unreviewed upstream release could break screenshot capture and LinkedIn
# session recovery on the next clean install, with nothing in this repo
# recording which version was ever known good.
#
# To bump: BROWSERKIT_REF=$(git -C ../../libs/py-browser-kit rev-parse origin/main)
# and update the default below. Set BROWSERKIT_PIN to override the requirement
# wholesale (e.g. `-e /path/to/py-browser-kit` when developing against a local
# checkout).
BROWSERKIT_REPO_HOST="${BROWSERKIT_REPO_HOST:-gitlab.flexinfer.ai}"
BROWSERKIT_REPO_PATH="${BROWSERKIT_REPO_PATH:-libs/py-browser-kit.git}"
# libs/py-browser-kit main @ 2026-07-02 (flexinfer-browser-kit 0.3.0)
BROWSERKIT_REF="${BROWSERKIT_REF:-78f17684fb8b1925dbcf6a857d8fffc5bdaafe16}"
BROWSERKIT_URL="https://${BROWSERKIT_REPO_HOST}/${BROWSERKIT_REPO_PATH}"

# libs/py-browser-kit is a private project. Use a CI job token when one is in
# the environment; otherwise fall back to the caller's git credential helper.
BROWSERKIT_AUTH=""
if [ -n "${CI_JOB_TOKEN:-}" ]; then
  BROWSERKIT_AUTH="gitlab-ci-token:${CI_JOB_TOKEN}@"
fi
BROWSERKIT_PIN="${BROWSERKIT_PIN:-flexinfer-browser-kit @ git+https://${BROWSERKIT_AUTH}${BROWSERKIT_REPO_HOST}/${BROWSERKIT_REPO_PATH}@${BROWSERKIT_REF}}"

# playwright already arrives transitively (browser-kit requires
# playwright>=1.40.0). It is named and pinned explicitly so a clean install
# cannot land on a driver the chromium launch check below has never run
# against.
PLAYWRIGHT_PIN="${PLAYWRIGHT_PIN:-playwright==1.58.0}"

echo "Installing Python deps for BrowserKit (pinned)..."
echo "  flexinfer-browser-kit @ git+${BROWSERKIT_URL}@${BROWSERKIT_REF}"
echo "  ${PLAYWRIGHT_PIN}"
# Deliberately not wrapped in `set -x`: BROWSERKIT_PIN can embed CI_JOB_TOKEN.
"$PY" -m pip install "$BROWSERKIT_PIN" "$PLAYWRIGHT_PIN"

echo "Installing Playwright Chromium (download)..."
set -x
"$PY" -m playwright install chromium
set +x

echo "Done."
echo ""
echo "Next:"
echo "  export BROWSERKIT_PYTHON=\"$PY\""
echo "  bash scripts/browserkit/check_ready.sh"
