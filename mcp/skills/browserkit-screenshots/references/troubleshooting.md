# Troubleshooting

## "No module named browser_kit" / "No module named playwright"

Install Python deps with the pinned installer (a bare `pip install
flexinfer-browser-kit` pulls an unpinned PyPI release of an internal library):

```bash
bash scripts/browserkit/install_deps.sh
```

## "Executable doesn't exist" / Chromium won't launch

Install Playwright browsers:

```bash
python3 -m playwright install chromium
```

## Screenshots Are Blank / Missing Data

- Try `wait_until: "networkidle"` and a small `wait_ms` (e.g. 200-500ms).
- If a CSS selector is present, make sure it's visible in the current viewport, or omit `selector`.

## Corporate DNS / SSL / Proxies

BrowserKit runs locally; use host network configuration. For internal services, prefer `http://localhost:...` or your VPN DNS name.
