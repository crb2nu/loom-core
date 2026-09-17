#!/usr/bin/env bash
set -euo pipefail

govulncheck_bin="${GOVULNCHECK_BIN:-$(go env GOPATH)/bin/govulncheck}"
# JSON mode does not fail on findings. Keep raw evidence and always run the
# native converter after the exact, expiring metadata correction.
"$govulncheck_bin" -scan=package -json ./... > govulncheck-report.raw.json
go run ./scripts/ci/govulncheck-filter govulncheck-report.raw.json govulncheck-report.filtered.json
"$govulncheck_bin" -mode=convert < govulncheck-report.filtered.json | tee govulncheck-report.txt
