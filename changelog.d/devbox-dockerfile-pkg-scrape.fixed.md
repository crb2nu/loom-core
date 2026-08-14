devbox: the sandbox Dockerfile generator no longer ingests tokens from chained
commands when scraping `apt-get install` / `apk add` lines out of a project's own
Dockerfile. Package extraction now stops at shell separators (`&&`, `;`, `|`, redirects)
and drops `$VAR` tokens, so a Dockerfile line like
`apk add --no-cache ca-certificates tzdata && adduser -D -u 1000 flexdeck` yields
`ca-certificates tzdata` instead of leaking `adduser 1000 flexdeck` into the generated
image's package list (which made `apk` fail with "1000 (no such package)" and broke
sandbox builds for flexdeck and its worktrees).
