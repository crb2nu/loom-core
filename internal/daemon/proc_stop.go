package daemon

// stopServerProc stops the local process for serverName. When the per-id
// stdio mux is enabled (LOOM_MUX_STDIO=1), the cached *muxstdio.Transport
// is evicted first so any in-flight callers receive ErrClosed before
// procMgr.Stop tears down the underlying pipe. Safe to call when muxStdio
// is disabled (muxCache is nil) or when no entry exists for serverName.
func (d *Daemon) stopServerProc(serverName string) error {
	if d == nil {
		return nil
	}
	if d.muxStdio && d.muxCache != nil {
		d.muxCache.Evict(serverName)
	}
	if d.procMgr == nil {
		return nil
	}
	return d.procMgr.Stop(serverName)
}

// stopAllServerProcs evicts every cached mux and stops every running
// process. Called during daemon shutdown.
func (d *Daemon) stopAllServerProcs() {
	if d == nil {
		return
	}
	if d.muxStdio && d.muxCache != nil {
		d.muxCache.CloseAll()
	}
	if d.procMgr != nil {
		d.procMgr.StopAll()
	}
}
