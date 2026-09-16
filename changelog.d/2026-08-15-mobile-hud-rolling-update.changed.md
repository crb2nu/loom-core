- mobile-hud: deployment strategy Recreate → RollingUpdate (maxSurge 1,
  maxUnavailable 0) with required same-node self-affinity so the surge pod
  shares the RWO workspace volume's node attachment. Every merged MR rolls the
  fleet; under Recreate each roll was a ~60–90s hud.flexinfer.ai outage (image
  pull + boot with no serving pod). The spawn-controller handoff overlap is
  bounded and fenced (per-entry owner conflicts, idempotent shepherd enqueues).
