- Upgraded `golang.org/x/crypto` to v0.56.0 for GO-2026-6354 and GO-2026-6355
  (denial of service on deadlocked SSH channels in `x/crypto/ssh`). The
  advisories were published 2026-09-02 and failed `security:govulncheck` on
  every main pipeline, which held back the day's image builds.
