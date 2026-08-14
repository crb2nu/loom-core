package killtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	s1cOperatorNamespace    = "loom-mills"
	s1cSpawnNamespace       = "devbox"
	policyConfigMapName     = "loom-mills-policy"
	spawnStateConfigMapName = "loom-spawn-state"
)

// KubernetesObjectIdentity is the serialized API-server identity shared by
// crash-critical namespaced objects. ResourceVersion detects in-place writes;
// UID distinguishes replacement under the same namespace/name.
type KubernetesObjectIdentity struct {
	Name              string `json:"name"`
	Namespace         string `json:"namespace"`
	UID               string `json:"uid"`
	ResourceVersion   string `json:"resource_version"`
	Terminating       bool   `json:"terminating"`
	DeletionTimestamp string `json:"deletion_timestamp"`
}

type kubernetesObjectMetadataWire struct {
	Name              string  `json:"name"`
	Namespace         string  `json:"namespace"`
	UID               string  `json:"uid"`
	ResourceVersion   string  `json:"resourceVersion"`
	DeletionTimestamp *string `json:"deletionTimestamp"`
}

type configMapObjectWire struct {
	Metadata kubernetesObjectMetadataWire `json:"metadata"`
	Data     map[string]string            `json:"data"`
}

// ValidateKubernetesObjectIdentity fails closed unless identity is the exact,
// live API object expected by the caller.
func ValidateKubernetesObjectIdentity(
	identity KubernetesObjectIdentity,
	expectedNamespace, expectedName string,
) error {
	if identity.Name != expectedName {
		return fmt.Errorf("kubernetes object name %q differs from expected %q", identity.Name, expectedName)
	}
	if identity.Namespace != expectedNamespace {
		return fmt.Errorf("kubernetes object namespace %q differs from expected %q", identity.Namespace, expectedNamespace)
	}
	if strings.TrimSpace(identity.UID) == "" {
		return errors.New("kubernetes object UID is missing")
	}
	if strings.TrimSpace(identity.ResourceVersion) == "" {
		return errors.New("kubernetes object resourceVersion is missing")
	}
	if identity.Terminating || strings.TrimSpace(identity.DeletionTimestamp) != "" {
		return fmt.Errorf("kubernetes object %s/%s is terminating at %q",
			identity.Namespace, identity.Name, identity.DeletionTimestamp)
	}
	return nil
}

func parseConfigMapObject(
	raw, expectedNamespace, expectedName string,
) (KubernetesObjectIdentity, map[string]string, error) {
	var object configMapObjectWire
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return KubernetesObjectIdentity{}, nil, err
	}
	identity := KubernetesObjectIdentity{
		Name: object.Metadata.Name, Namespace: object.Metadata.Namespace,
		UID: object.Metadata.UID, ResourceVersion: object.Metadata.ResourceVersion,
		Terminating: object.Metadata.DeletionTimestamp != nil,
	}
	if object.Metadata.DeletionTimestamp != nil {
		identity.DeletionTimestamp = *object.Metadata.DeletionTimestamp
	}
	if err := ValidateKubernetesObjectIdentity(identity, expectedNamespace, expectedName); err != nil {
		return identity, nil, err
	}
	return identity, object.Data, nil
}

func parsePolicyConfigMap(raw, expectedNamespace string) (KubernetesObjectIdentity, string, error) {
	identity, data, err := parseConfigMapObject(raw, expectedNamespace, policyConfigMapName)
	if err != nil {
		return identity, "", err
	}
	policy, ok := data["policy.yaml"]
	if !ok {
		return identity, "", errors.New(`policy ConfigMap data is missing "policy.yaml"`)
	}
	return identity, policy, nil
}

func sameKubernetesObjectStableIdentity(left, right KubernetesObjectIdentity) bool {
	return left.Name == right.Name && left.Namespace == right.Namespace && left.UID == right.UID
}

// ValidateSpawnStateSnapshotIdentity cross-binds the legacy UID field to the
// full object identity retained by every live spawn-state collection.
func ValidateSpawnStateSnapshotIdentity(snapshot SpawnStateSnapshot, expectedNamespace string) error {
	if err := ValidateKubernetesObjectIdentity(
		snapshot.ConfigMapIdentity, expectedNamespace, spawnStateConfigMapName,
	); err != nil {
		return err
	}
	if strings.TrimSpace(snapshot.ConfigMapUID) == "" || snapshot.ConfigMapUID != snapshot.ConfigMapIdentity.UID {
		return fmt.Errorf("legacy spawn ConfigMap UID %q differs from object identity UID %q",
			snapshot.ConfigMapUID, snapshot.ConfigMapIdentity.UID)
	}
	return nil
}

// ValidatePreflightConfigMapIdentities proves both crash-critical ConfigMaps
// are live exact objects and that the legacy spawn UID cannot diverge from the
// serialized identity.
func ValidatePreflightConfigMapIdentities(report PreflightReport) error {
	if err := ValidateKubernetesObjectIdentity(
		report.PolicyConfigMapIdentity, s1cOperatorNamespace, policyConfigMapName,
	); err != nil {
		return fmt.Errorf("policy ConfigMap identity: %w", err)
	}
	spawn := SpawnStateSnapshot{
		ConfigMapUID: report.SpawnConfigMapUID, ConfigMapIdentity: report.SpawnConfigMapIdentity,
	}
	if err := ValidateSpawnStateSnapshotIdentity(spawn, s1cSpawnNamespace); err != nil {
		return fmt.Errorf("durable spawn ConfigMap identity: %w", err)
	}
	return nil
}

// ValidateConfigMapGateIdentity freezes the policy object completely,
// including resourceVersion, while allowing the durable spawn ConfigMap's
// resourceVersion to advance as records change. Namespace/name/UID remain
// immutable for both objects.
func ValidateConfigMapGateIdentity(initial, observed PreflightReport) error {
	if err := ValidatePreflightConfigMapIdentities(initial); err != nil {
		return fmt.Errorf("initial preflight: %w", err)
	}
	if err := ValidatePreflightConfigMapIdentities(observed); err != nil {
		return fmt.Errorf("observed preflight: %w", err)
	}
	if initial.PolicyConfigMapIdentity != observed.PolicyConfigMapIdentity {
		return fmt.Errorf("policy ConfigMap identity changed: %+v -> %+v",
			initial.PolicyConfigMapIdentity, observed.PolicyConfigMapIdentity)
	}
	if !sameKubernetesObjectStableIdentity(initial.SpawnConfigMapIdentity, observed.SpawnConfigMapIdentity) {
		return fmt.Errorf("durable spawn ConfigMap stable identity changed: %+v -> %+v",
			initial.SpawnConfigMapIdentity, observed.SpawnConfigMapIdentity)
	}
	return nil
}
