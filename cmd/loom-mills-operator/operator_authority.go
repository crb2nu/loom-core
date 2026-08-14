package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const (
	operatorAuthorityContract        = "mills-s1c-operator-authority"
	operatorAuthorityContractVersion = 1

	operatorAuthorityContractHeader       = "X-Loom-Mills-Authority-Contract"
	operatorAuthorityVersionHeader        = "X-Loom-Mills-Authority-Version"
	operatorAuthorityPodNameHeader        = "X-Loom-Mills-Pod-Name"
	operatorAuthorityPodNamespaceHeader   = "X-Loom-Mills-Pod-Namespace"
	operatorAuthorityPodUIDHeader         = "X-Loom-Mills-Pod-Uid"
	operatorAuthorityDeploymentNameHeader = "X-Loom-Mills-Deployment-Name"
	operatorAuthorityBootIDHeader         = "X-Loom-Mills-Boot-Id"
)

const operatorAuthorityBootIDBytes = 32

// operatorAuthorityIdentity is the process identity attested on every REST
// response. Kubernetes supplies the Pod fields through the Downward API; the
// kill-test binds them to its independently read Pod -> ReplicaSet ->
// Deployment owner chain before it accepts any response as crash evidence.
type operatorAuthorityIdentity struct {
	PodName        string
	PodNamespace   string
	PodUID         string
	DeploymentName string
	BootID         string
}

func operatorAuthorityBootID(reader io.Reader) string {
	value := make([]byte, operatorAuthorityBootIDBytes)
	if _, err := io.ReadFull(reader, value); err != nil {
		return ""
	}
	return hex.EncodeToString(value)
}

func operatorAuthorityIdentityFromEnv() operatorAuthorityIdentity {
	deploymentName := strings.TrimSpace(os.Getenv("LOOM_MILLS_DEPLOYMENT_NAME"))
	if deploymentName == "" {
		deploymentName = "loom-mills-operator"
	}
	return operatorAuthorityIdentity{
		PodName:        strings.TrimSpace(os.Getenv("POD_NAME")),
		PodNamespace:   strings.TrimSpace(os.Getenv("POD_NAMESPACE")),
		PodUID:         strings.TrimSpace(os.Getenv("POD_UID")),
		DeploymentName: deploymentName,
		// RNG failure must not take the operator down. An empty header makes
		// the destructive S1c gate fail closed while normal service continues.
		BootID: operatorAuthorityBootID(rand.Reader),
	}
}

func safeAuthorityHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

// withOperatorAuthority makes backend identity part of every response,
// including authorization and error responses emitted before a JSON handler.
// Incomplete Downward API wiring is deliberately visible as missing fields;
// the S1c harness then fails closed rather than accepting an unattested pod.
func (o *operator) withOperatorAuthority(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(operatorAuthorityContractHeader, operatorAuthorityContract)
		w.Header().Set(operatorAuthorityVersionHeader, strconv.Itoa(operatorAuthorityContractVersion))
		w.Header().Set(operatorAuthorityPodNameHeader, safeAuthorityHeaderValue(o.authority.PodName))
		w.Header().Set(operatorAuthorityPodNamespaceHeader, safeAuthorityHeaderValue(o.authority.PodNamespace))
		w.Header().Set(operatorAuthorityPodUIDHeader, safeAuthorityHeaderValue(o.authority.PodUID))
		w.Header().Set(operatorAuthorityDeploymentNameHeader, safeAuthorityHeaderValue(o.authority.DeploymentName))
		w.Header().Set(operatorAuthorityBootIDHeader, safeAuthorityHeaderValue(o.authority.BootID))
		next.ServeHTTP(w, r)
	})
}
