Allow the mobile operator token to reach `GET /api/health`. The endpoint is
served unauthenticated to everyone, so the scope guard's 403 added no
protection — it only broke the mills operator's sentinel "hud" liveness probe,
which presents `LOOM_HUD_TOKEN` (the mobile operator token) and had been
tripping a permanent incident with consecutive 403s.
