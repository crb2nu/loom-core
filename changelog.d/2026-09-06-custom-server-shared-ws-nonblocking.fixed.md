Stop the shared-child WebSocket handler in `custom-server` from starving
control frames. `MCP_SHARED_CHILD=1` (shipped for the hub's devbox server in
the previous change) ran the supervisor call inline in the read goroutine, so
for the whole duration of a call no ping or pong was processed. A devbox
quality-gate call runs for minutes, the Mills operator's transport pings and
expects a pong, and every call longer than its pong wait died at exactly 60s
with `websocket: close 1006 (abnormal closure): unexpected EOF` — no
tests-stage attempt passed in the six hours the image was live on 2026-09-06
(the canary alone burned 38). Requests are now dispatched off the read loop,
responses are written under the connection's write lock, and the handler waits
for in-flight calls before returning; a regression test pings the server
throughout a 1.5s call and requires the pongs and the response to arrive.
