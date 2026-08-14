package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperatorAuthorityHeadersBindEveryResponse(t *testing.T) {
	op := &operator{authority: operatorAuthorityIdentity{
		PodName: "operator-pod", PodNamespace: "loom-mills", PodUID: "pod-uid",
		DeploymentName: "loom-mills-operator",
		BootID:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	handler := op.withOperatorAuthority(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected", http.StatusConflict)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/mills/safety/quiescence", nil))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	wants := map[string]string{
		operatorAuthorityContractHeader:       operatorAuthorityContract,
		operatorAuthorityVersionHeader:        "1",
		operatorAuthorityPodNameHeader:        "operator-pod",
		operatorAuthorityPodNamespaceHeader:   "loom-mills",
		operatorAuthorityPodUIDHeader:         "pod-uid",
		operatorAuthorityDeploymentNameHeader: "loom-mills-operator",
		operatorAuthorityBootIDHeader:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	for header, want := range wants {
		if got := recorder.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestOperatorAuthorityBootIDFailsClosedWithoutStoppingOperator(t *testing.T) {
	if got := operatorAuthorityBootID(errorReader{}); got != "" {
		t.Fatalf("boot ID after RNG failure = %q, want empty", got)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("injected RNG failure") }

func TestOperatorAuthorityRejectsHeaderInjection(t *testing.T) {
	op := &operator{authority: operatorAuthorityIdentity{PodUID: "uid\r\nX-Forged: true"}}
	recorder := httptest.NewRecorder()
	op.withOperatorAuthority(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := recorder.Header().Get(operatorAuthorityPodUIDHeader); got != "" {
		t.Fatalf("unsafe Pod UID header = %q", got)
	}
}
