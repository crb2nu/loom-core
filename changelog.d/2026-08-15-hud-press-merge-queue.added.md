- HUD: the serial merge queue (`/api/mills/merge-queue`) now has an operator
  surface — a "press" section on the Bolts panel showing every active lane
  entry (per-lane position, state, MR link, attempts, age) with honest idle /
  disabled-by-policy / feed-failed states, plus a lane-depth instrument on the
  Factory rail. The endpoint previously had no frontend consumer, so per-lane
  rebase→re-prove→merge progress was invisible.
