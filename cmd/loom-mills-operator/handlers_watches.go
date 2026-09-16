package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type watchRegisterRequest struct {
	SubjectKind       store.WatchSubjectKind `json:"subject_kind"`
	SubjectID         string                 `json:"subject_id"`
	TerminalCondition string                 `json:"terminal_condition"`
	Note              string                 `json:"note"`
	TTLSeconds        int64                  `json:"ttl_seconds"`
}

func (o *operator) handleWatchesList(w http.ResponseWriter, r *http.Request) {
	f := store.WatchFilter{State: store.WatchState(r.URL.Query().Get("state")), SubjectKind: store.WatchSubjectKind(r.URL.Query().Get("subject_kind")), SubjectID: strings.TrimSpace(r.URL.Query().Get("subject_id"))}
	if f.State != "" && f.State != store.WatchActive && f.State != store.WatchMet && f.State != store.WatchExpired && f.State != store.WatchCancelled {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	if f.SubjectKind != "" && !validWatchSubjectKind(f.SubjectKind) {
		http.Error(w, "invalid subject_kind", http.StatusBadRequest)
		return
	}
	items, err := o.store.Watches.List(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (o *operator) handleWatchRegister(w http.ResponseWriter, r *http.Request) {
	var req watchRegisterRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), 400)
		return
	}
	req.SubjectID, req.TerminalCondition = strings.TrimSpace(req.SubjectID), strings.ToLower(strings.TrimSpace(req.TerminalCondition))
	if err := store.ValidateWatchSubject(req.SubjectKind, req.SubjectID, req.TerminalCondition); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	maxTTLSeconds := int64(store.MaxWatchTTL / time.Second)
	if req.TTLSeconds <= 0 || req.TTLSeconds > maxTTLSeconds {
		http.Error(w, "ttl_seconds must be between 1 and 31536000", 400)
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	now := time.Now().UTC()
	item := &store.Watch{SubjectKind: req.SubjectKind, SubjectID: req.SubjectID, TerminalCondition: req.TerminalCondition, Note: req.Note, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := o.store.Watches.Register(r.Context(), item); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (o *operator) handleWatchResolve(w http.ResponseWriter, r *http.Request) {
	o.finishWatch(w, r, store.WatchMet, "resolved by operator")
}
func (o *operator) handleWatchCancel(w http.ResponseWriter, r *http.Request) {
	o.finishWatch(w, r, store.WatchCancelled, "cancelled by operator")
}

func (o *operator) finishWatch(w http.ResponseWriter, r *http.Request, state store.WatchState, fallback string) {
	var body struct {
		Resolution string `json:"resolution"`
	}
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid body: "+err.Error(), 400)
			return
		}
	}
	if strings.TrimSpace(body.Resolution) == "" {
		body.Resolution = fallback
	}
	won, err := o.store.Watches.Resolve(r.Context(), r.PathValue("id"), state, body.Resolution, time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "watch not found", 404)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	item, err := o.store.Watches.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	status := http.StatusOK
	if !won {
		status = http.StatusConflict
	}
	writeJSON(w, status, item)
}

func validWatchSubjectKind(k store.WatchSubjectKind) bool {
	return k == store.WatchSubjectBacklogItem || k == store.WatchSubjectMergeRequest || k == store.WatchSubjectPipelineRun
}
