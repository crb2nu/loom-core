package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/webhookbus"
)

type gitLabWebhookPayload struct {
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		SHA        string `json:"sha"`
		IID        int64  `json:"iid"`
		LastCommit struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
}

func (o *operator) handleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	provided := r.Header.Get("X-Gitlab-Token")
	providedHash := sha256.Sum256([]byte(provided))
	wantHash := sha256.Sum256([]byte(o.webhookSecret))
	if o.webhookSecret == "" || subtle.ConstantTimeCompare(providedHash[:], wantHash[:]) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	eventType := r.Header.Get("X-Gitlab-Event")
	if eventType != "Pipeline Hook" && eventType != "Merge Request Hook" {
		w.WriteHeader(http.StatusOK)
		return
	}
	var payload gitLabWebhookPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
		http.Error(w, "malformed webhook payload", http.StatusBadRequest)
		return
	}
	event := webhookbus.Event{Project: strings.TrimSpace(payload.Project.PathWithNamespace)}
	if eventType == "Pipeline Hook" {
		event.SHA = strings.TrimSpace(payload.ObjectAttributes.SHA)
	} else {
		event.MRIID = payload.ObjectAttributes.IID
		event.SHA = strings.TrimSpace(payload.ObjectAttributes.LastCommit.ID)
	}
	if event.Project == "" || (event.SHA == "" && event.MRIID == 0) {
		http.Error(w, "malformed webhook payload", http.StatusBadRequest)
		return
	}
	if o.webhookBus != nil {
		o.webhookBus.Publish(event)
	}
	w.WriteHeader(http.StatusOK)
}
