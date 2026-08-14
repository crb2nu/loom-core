import { EventTicker } from "./components/EventTicker";
import { HandoffInbox } from "./components/HandoffInbox";
import { useFleet, type BlockedSession } from "./hooks/useFleet";
import { hostKind } from "./lib/mcpBridge";

// FleetOverview renders the loom fleet dashboard inline. Data flows
// widget → host (Claude/ChatGPT) → mcp-loom-widget (Go) → loom HUD.
// The bearer token lives only in the Go process, per slice 1b-γ.
export function FleetOverview() {
  const { data, error, loading, lastUpdated } = useFleet();
  const isMock = hostKind() === "mock";

  return (
    <div className="card">
      <h1>
        <span
          className={data?.daemon_running ? "dot dot-ok" : "dot dot-warn"}
          aria-hidden="true"
        />
        Loom Fleet
        {isMock && <span className="badge">preview · mock data</span>}
      </h1>
      <p className="sub">{summary(data, loading, error)}</p>

      {error && <Banner kind="error">{error}</Banner>}

      {data && (
        <>
          <Row label="Daemon" value={data.daemon_running ? "running" : "down"} />
          <Row label="Active sessions" value={String(data.active_sessions)} />
          <Row
            label="Agents"
            value={`${data.active_agents} active · ${data.idle_agents} idle · ${data.offline_agents} offline`}
          />
          <BlockedPanel
            count={data.blocked_count ?? 0}
            sessions={data.blocked ?? []}
          />
          <Row label="MCP servers" value={String(data.server_count)} />
          {data.health && (
            <Row
              label="Server health"
              value={`${data.health.healthy_servers ?? 0} healthy · ${data.health.degraded_servers ?? 0} degraded · ${data.health.down_servers ?? 0} down`}
            />
          )}
          {data.spawns && (
            <Row
              label="Spawns"
              value={`${data.spawns.active ?? 0} active · ${data.spawns.total ?? 0} total`}
            />
          )}
          {data.last_heartbeat?.agent_id && (
            <Row
              label="Last heartbeat"
              value={`${data.last_heartbeat.agent_id} · ${data.last_heartbeat.count_1h ?? 0}/h`}
            />
          )}
        </>
      )}

      <HandoffInbox />

      <EventTicker />

      <p className="footer">
        {lastUpdated ? `updated ${lastUpdated.toLocaleTimeString()}` : "polling…"}
        {" · "}
        <code>loom_fleet_get_dashboard</code>
      </p>
    </div>
  );
}

function summary(
  data: ReturnType<typeof useFleet>["data"],
  loading: boolean,
  error: string | null
): string {
  if (error) return "could not reach loom HUD";
  if (loading && !data) return "fetching loom fleet…";
  if (!data) return "no data";
  return `${data.active_agents + data.idle_agents + data.offline_agents} agents tracked, ${data.active_sessions} live sessions`;
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="row">
      <span className="label">{label}</span>
      <span className="value">{value}</span>
    </div>
  );
}

function Banner({ kind, children }: { kind: "error" | "info"; children: React.ReactNode }) {
  return <div className={`banner banner-${kind}`}>{children}</div>;
}

// BlockedPanel surfaces sessions waiting on a human (flightdeck-derived
// permission stalls). It renders nothing when nobody is blocked, so the
// dashboard stays quiet until an agent actually needs attention.
function BlockedPanel({
  count,
  sessions,
}: {
  count: number;
  sessions: BlockedSession[];
}) {
  if (count <= 0) return null;
  return (
    <>
      <div className="row">
        <span className="label">
          <span className="dot dot-warn" aria-hidden="true" /> Blocked
        </span>
        <span className="value">
          <strong>{count}</strong> waiting on a human
        </span>
      </div>
      {sessions.map((s) => (
        <div className="row" key={s.session_id} title={s.cwd || s.session_id}>
          <span className="label" style={{ paddingLeft: "1rem", opacity: 0.8 }}>
            {shortId(s.session_id)}
            {s.tool_name ? ` · ${s.tool_name}` : ""}
          </span>
          <span className="value">{fmtWaited(s.waited_seconds)}</span>
        </div>
      ))}
    </>
  );
}

function shortId(id: string): string {
  return id ? id.slice(0, 8) : "—";
}

function fmtWaited(secs?: number): string {
  if (secs == null || secs < 0) return "—";
  if (secs < 60) return `${secs}s`;
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m}m ${secs % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}
