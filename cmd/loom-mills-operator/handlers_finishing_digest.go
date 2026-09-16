package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/finishing/digest"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// handleFinishingDigest serves a stored digest: ?day=YYYY-MM-DD reads that
// UTC day (the append-once subject), no day reads the newest one. Both are
// single-row index seeks; nothing is composed on the read path.
func (o *operator) handleFinishingDigest(w http.ResponseWriter, r *http.Request) {
	day := strings.TrimSpace(r.URL.Query().Get("day"))
	var (
		e   *store.Event
		err error
	)
	if day == "" {
		e, err = o.store.Events.LatestByKind(r.Context(), finishingDigestKind)
	} else {
		if _, parseErr := time.Parse("2006-01-02", day); parseErr != nil {
			http.Error(w, "invalid day", http.StatusBadRequest)
			return
		}
		e, err = o.store.Events.FirstBySubjectKind(r.Context(), finishingDigestKind, day, finishingDigestKind)
	}
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "digest not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d, err := digestFromEvent(e)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleFinishingDigestRun composes ?day=YYYY-MM-DD (default: yesterday, the
// last completed UTC day) through the scheduler's exact path. A day that has
// not ended is refused with 400 so a partial ledger can never become that
// day's only digest; an existing day answers inserted=false and delivers no
// hook twice; a stored digest whose hook delivery failed still answers 200,
// with delivery_error set, because the durable event is authoritative.
func (o *operator) handleFinishingDigestRun(w http.ResponseWriter, r *http.Request) {
	day := o.shiftNow().UTC().AddDate(0, 0, -1)
	if raw := strings.TrimSpace(r.URL.Query().Get("day")); raw != "" {
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			http.Error(w, "invalid day", http.StatusBadRequest)
			return
		}
		day = parsed
	}
	d, inserted, err := o.runFinishingDigest(r.Context(), day)
	if errors.Is(err, errDigestDayIncomplete) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil && !inserted {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := map[string]any{"digest": d, "inserted": inserted}
	if err != nil {
		body["delivery_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// digestFromEvent decodes the stored payload back into the API shape via
// JSON, the inverse of digestPayload.
func digestFromEvent(e *store.Event) (digest.Digest, error) {
	b, err := json.Marshal(e.Payload)
	if err != nil {
		return digest.Digest{}, err
	}
	var d digest.Digest
	if err := json.Unmarshal(b, &d); err != nil {
		return digest.Digest{}, err
	}
	return d, nil
}
