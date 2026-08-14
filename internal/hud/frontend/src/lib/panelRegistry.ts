// Data-driven sub-view → panel mapping.
//
// Every panel is a lazy thunk so Vite code-splits each one into its own
// chunk and App.svelte stays a thin shell: adding a panel means adding a
// registry entry (plus a ViewDef sub-view in router.svelte.ts), not a new
// import + template branch. Thunks must stay statically analyzable
// (`() => import('...literal...')`) for Vite to see them.
//
// Keys are router sub-view IDs. `spawn-detail` is the one non-sub-view key:
// App.svelte selects it over `spawn` when router.detail is set.

export type PanelLoader = () => Promise<{ default: unknown }>;

export const panelLoaders: Record<string, PanelLoader> = {
  // operator — the unified Operator Deck (landing view). Sub-view id is
  // `deck`; the view has no other sub-views so ViewShell hides the tab bar.
  deck: () => import('./components/OperatorPanel.svelte'),

  // overview — the standalone "Now" panel. Not a sub-view key: App.svelte
  // selects it when router.view === overviewId. Kept lazy so it stays out of
  // the entry chunk (the landing view is operator/deck, not overview).
  overview: () => import('./components/OverviewPanel.svelte'),

  // agents
  fleet:     () => import('./components/FleetPanel.svelte'),
  // Unified cross-vendor session browser (claude + codex transcripts from
  // every host, joined against the live fleet).
  sessions:  () => import('./components/SessionsPanel.svelte'),
  dispatch:  () => import('./components/DispatchPanel.svelte'),
  presence:  () => import('./components/PresencePanel.svelte'),
  topology:  () => import('./components/TopologyPanel.svelte'),
  lifecycle: () => import('./components/LifecyclePanel.svelte'),
  mrwatch:   () => import('./components/MRWatchPanel.svelte'),
  alerts:    () => import('./components/AlertsPanel.svelte'),

  // infra
  servers: () => import('./components/ServersPanel.svelte'),
  catalog: () => import('./components/CatalogPanel.svelte'),
  weaver:  () => import('./components/WeaverPanel.svelte'),

  // tasks
  tasks:     () => import('./components/TasksPanel.svelte'),
  workflows: () => import('./components/WorkflowsPanel.svelte'),
  plans:     () => import('./components/PlansPanel.svelte'),
  // Non-sub-view key: App.svelte selects it over `plans` when router.detail
  // encodes 2+ compared plan ids (`id1+id2`).
  'plans-compare': () => import('./components/PlansComparePanel.svelte'),

  // knowledge
  feed:      () => import('./components/KnowledgePanel.svelte'),
  memory:    () => import('./components/MemoryPanel.svelte'),
  graph:     () => import('./components/GraphPanel.svelte'),
  reasoning: () => import('./components/ReasoningPanel.svelte'),
  'context-health': () => import('./components/ContextHealthPanel.svelte'),

  // activity
  timeline: () => import('./components/TimelinePanel.svelte'),
  stream:   () => import('./components/StreamPanel.svelte'),
  traces:   () => import('./components/TracesPanel.svelte'),

  // sandbox
  sandbox:        () => import('./components/SandboxPanel.svelte'),
  spawn:          () => import('./components/SpawnPanel.svelte'),
  'spawn-detail': () => import('./components/SpawnDetailPanel.svelte'),

  // tasks (cont.) — Projects is a Work sub-view; it was a top-level view until
  // it was folded in as the per-project lens over Plans.
  projects:  () => import('./components/ProjectsPanel.svelte'),

  // mills
  // `mills-overview`, not `overview`: the bare id is the standalone top-level
  // Overview ("Now") panel. Same disambiguation as `mills-workflows`.
  'mills-overview':  () => import('./components/mills/MillsOverviewPanel.svelte'),
  factory:           () => import('./components/mills/FactoryPanel.svelte'),
  // Mill-floor spine (spec S0/S6). These replaced the retired Backlog + Pipelines
  // tabs; the legacy `backlog`/`pipelines` hashes redirect to warps/shuttles via
  // router.svelte.ts. The old panels were deleted in S6.
  warps:             () => import('./components/mills/WarpsPanel.svelte'),
  shuttles:          () => import('./components/mills/ShuttlesPanel.svelte'),
  sparks:            () => import('./components/mills/SparksPanel.svelte'),
  bolts:             () => import('./components/mills/BoltsPanel.svelte'),
  telemetry:         () => import('./components/mills/TelemetryPanel.svelte'),
  'mills-workflows': () => import('./components/mills/WorkflowsPanel.svelte'),
  // `staff` is the Mill Staff group's landing panel: the three departments in
  // one view plus the five staff evidence reports.
  staff:             () => import('./components/mills/MillStaffPanel.svelte'),
  council:           () => import('./components/mills/CouncilPanel.svelte'),
  eval:              () => import('./components/mills/EvalPanel.svelte'),
  squads:            () => import('./components/mills/SquadsPanel.svelte'),
  audit:             () => import('./components/mills/AuditPanel.svelte'),
  'cross-repo':      () => import('./components/mills/CrossRepoPanel.svelte'),
  policy:            () => import('./components/mills/PolicyPanel.svelte'),
  overseers:         () => import('./components/mills/OverseersPanel.svelte'),
  patterns:          () => import('./components/mills/PatternsPanel.svelte'),
};
