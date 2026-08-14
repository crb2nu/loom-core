Stop the HUD status bar lying on cold loads. The footer renders fleet-derived
facts (connection word, server/agent/session counts) on every route, but only
fleet-reading views started the fleet poll — a cold load on a non-fleet route
(e.g. `#mills/staff`) showed "Disconnected · 0 servers · 0 live agents" over a
live daemon, and "42/0 healthy" because the healthy ratio paired healthStore's
numerator with fleetStore's never-fetched denominator. The app shell now
registers as a slow (2-minute) fleet polling owner — owner semantics mean the
first owner triggers exactly one fetch and activates the SSE snapshot feed, so
views mounting later add no duplicate requests. Before the first snapshot the
bar shows a neutral "Connecting" instead of a red "Disconnected", count
segments stay hidden rather than asserting zeros, and the healthy ratio takes
both numbers from the health store.
