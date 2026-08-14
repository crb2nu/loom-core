package daemon

import (
	"context"
	"errors"
	"time"
)

// stopServerProc stops the local process for serverName. When the per-id
// stdio mux is enabled (LOOM_MUX_STDIO=1), the cached *muxstdio.Transport
// is evicted first so any in-flight callers receive ErrClosed before
// procMgr.Stop tears down the underlying pipe. Safe to call when muxStdio
// is disabled (muxCache is nil) or when no entry exists for serverName.
func (d *Daemon) stopServerProc(serverName string) error {
	if d == nil {
		return nil
	}
	if d.serverSupervisor != nil {
		retired, err := d.serverSupervisor.retireCurrent(serverName)
		if retired && d.pool != nil {
			d.pool.ClearServer(serverName)
		}
		return err
	}
	if d.muxStdio && d.muxCache != nil {
		d.muxCache.Evict(serverName)
	}
	if d.procMgr == nil {
		return nil
	}
	return d.procMgr.Stop(serverName)
}

// failServerGeneration retires only the local generation observed by the
// failing call. A delayed error from an older generation is a no-op and must
// not clear the current generation's idle pool entries.
func (d *Daemon) failServerGeneration(serverName string, generationID uint64, cause error) (bool, error) {
	if d == nil {
		return false, nil
	}
	if d.serverSupervisor == nil {
		if d.pool != nil {
			d.pool.ClearServer(serverName)
		}
		return true, d.stopServerProc(serverName)
	}
	if generationID == 0 {
		return false, nil
	}
	retired, err := d.serverSupervisor.failIfCurrent(serverName, generationID, cause)
	if retired && d.pool != nil {
		d.pool.ClearServer(serverName)
	}
	return retired, err
}

func (d *Daemon) stopServerGeneration(serverName string, generationID uint64) (bool, error) {
	if d == nil {
		return false, nil
	}
	if d.serverSupervisor == nil {
		return true, d.stopServerProc(serverName)
	}
	if generationID == 0 {
		return false, nil
	}
	snapshot, ok := d.serverSupervisor.current(serverName)
	if !ok || snapshot.Generation != generationID {
		return false, nil
	}
	retired, err := d.serverSupervisor.failIfCurrent(serverName, generationID, nil)
	if retired && d.pool != nil {
		d.pool.ClearServer(serverName)
	}
	return retired, err
}

// stopAllServerProcs evicts every cached mux and stops every running
// process. Called during daemon shutdown.
func (d *Daemon) stopAllServerProcs() {
	if d == nil {
		return
	}
	if d.serverSupervisor != nil {
		d.serverSupervisor.beginDrain()
		shutdownTimeout := d.serverShutdownTimeout
		if shutdownTimeout <= 0 {
			shutdownTimeout = 10 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		err := d.serverSupervisor.shutdown(ctx)
		cancel()
		if err != nil {
			d.stopErr = errors.Join(d.stopErr, err)
			if d.logger != nil {
				d.logger.Warn("server supervisor shutdown incomplete; leaving canceled connector teardown tracked", "error", err)
			}
		}
		// Do not fall through to the name-keyed Manager.StopAll here. A remote
		// connector can be stuck inside fi-mcp-kit's SSH handshake while holding
		// the manager mutex; calling StopAll after the supervisor deadline would
		// make daemon shutdown unbounded. Every successfully published process is
		// already owned and closed by a tracked supervisor resource.
		return
	}
	if d.muxStdio && d.muxCache != nil {
		d.muxCache.CloseAll()
	}
	if d.procMgr != nil {
		d.procMgr.StopAll()
	}
}
