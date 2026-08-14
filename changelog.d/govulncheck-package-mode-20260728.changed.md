CI: `security:govulncheck` now runs `-scan=package` instead of the default
symbol mode, and `github.com/klauspost/compress` is bumped v1.18.6 → v1.18.7
for GO-2026-5841 (s2 out-of-bounds read) which the stricter mode surfaced.

Symbol mode peaks at 6.9–8.4 GiB building SSA for the whole module, against
a hard 8Gi container ceiling that CI config cannot raise — the runners cap
`memory_limit_overwrite_max_allowed` at 8Gi — so it OOMKilled main
repeatedly and blocked every image build behind the deploy stage. Package
mode needs 0.07 GiB and 1.3s.

This tightens the gate rather than loosening it: package mode also fails on
vulnerabilities in packages we import but never call, which symbol mode
passed over. It does not reintroduce module mode's false reds — advisories
against merely-required modules (GO-2026-5932, x/crypto/openpgp, no fix
available) stay green.
