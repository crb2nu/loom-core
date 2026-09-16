- **Mills operator**: `POST /api/mills/council/run` (and `/dryrun`) no longer
  cancels the council when the HTTP client disconnects. A CLI that gave up
  waiting cancelled the editor mid-flight and left an `error` run behind
  after real reviewer spend; the run is now bounded only by the 10-minute
  request budget and operator shutdown, and a disconnected caller reads the
  outcome from `GET /api/mills/council/runs`.
