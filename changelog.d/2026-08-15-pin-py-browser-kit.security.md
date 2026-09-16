- **BrowserKit's Python dependency is SHA-pinned instead of pulled unpinned from
  PyPI** (`scripts/browserkit/install_deps.sh`): the installer ran
  `pip install -U flexinfer-browser-kit playwright`, which resolved whatever the
  public index happened to be serving. `flexinfer-browser-kit` is an internal
  library (`libs/py-browser-kit`), and two MCP servers import it at runtime —
  `cmd/mcp-browserkit` (`screenshot_helper.py`) and `cmd/mcp-linkedin`
  (`browserkit_helper.py`), both doing
  `from browser_kit.browser import BrowserConfig, BrowserManager` — so an
  unreviewed upstream release could break screenshot capture and LinkedIn
  session recovery on the next clean install, with nothing in this repo
  recording which version had ever been known good. It now installs the PEP 508
  direct reference
  `flexinfer-browser-kit @ git+https://gitlab.flexinfer.ai/libs/py-browser-kit.git@78f1768`
  (main as of 2026-07-02, version 0.3.0) per the "Internal Dependency Pinning"
  rule in `libs/STANDARDS.md`, with `playwright` pinned to `==1.58.0` alongside
  it. `BROWSERKIT_REF` bumps the SHA and `BROWSERKIT_PIN` overrides the whole
  requirement for local development against a checkout; `CI_JOB_TOKEN` is used
  for auth when present, and that install line is deliberately kept out of
  `set -x` so the token cannot reach the log. Verified by a clean-venv install
  of the pinned reference followed by both helper imports. Every remaining
  `pip install flexinfer-browser-kit` instruction — the `mcp-browserkit` package
  doc and `NotConfigured` remediation text, `check_ready.sh`, the
  browserkit-screenshots skill references and registry entry, and the LinkedIn
  session recovery runbook — now points at `install_deps.sh`, so following the
  docs cannot reintroduce an unpinned install.
