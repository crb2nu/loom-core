package killtest

import (
	"encoding/json"
	"strings"
	"testing"
)

func testConfigMapJSON(
	namespace, name, uid, resourceVersion string,
	data map[string]string,
	deletionTimestamp *string,
) string {
	object := configMapObjectWire{
		Metadata: kubernetesObjectMetadataWire{
			Name: name, Namespace: namespace, UID: uid, ResourceVersion: resourceVersion,
			DeletionTimestamp: deletionTimestamp,
		},
		Data: data,
	}
	raw, _ := json.Marshal(object)
	return string(raw)
}

func testSpawnStateConfigMapJSON(data map[string]string) string {
	return testConfigMapJSON(s1cSpawnNamespace, spawnStateConfigMapName, "spawn-state-uid", "100", data, nil)
}

func TestValidateKubernetesObjectIdentity(t *testing.T) {
	valid := KubernetesObjectIdentity{
		Name: "loom-mills-policy", Namespace: "loom-mills", UID: "policy-uid", ResourceVersion: "101",
	}
	if err := ValidateKubernetesObjectIdentity(valid, "loom-mills", "loom-mills-policy"); err != nil {
		t.Fatalf("valid identity rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*KubernetesObjectIdentity)
		want   string
	}{
		{"wrong name", func(identity *KubernetesObjectIdentity) { identity.Name = "other" }, "name"},
		{"wrong namespace", func(identity *KubernetesObjectIdentity) { identity.Namespace = "other" }, "namespace"},
		{"missing uid", func(identity *KubernetesObjectIdentity) { identity.UID = "" }, "UID"},
		{"missing resource version", func(identity *KubernetesObjectIdentity) { identity.ResourceVersion = "" }, "resourceVersion"},
		{"terminating flag", func(identity *KubernetesObjectIdentity) { identity.Terminating = true }, "terminating"},
		{"deletion timestamp", func(identity *KubernetesObjectIdentity) { identity.DeletionTimestamp = "2026-07-14T12:00:00Z" }, "terminating"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := valid
			tt.mutate(&identity)
			err := ValidateKubernetesObjectIdentity(identity, "loom-mills", "loom-mills-policy")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateKubernetesObjectIdentity() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParsePolicyConfigMapUsesProductionObjectEnvelope(t *testing.T) {
	raw := `{
		"apiVersion":"v1","kind":"ConfigMap",
		"metadata":{"name":"loom-mills-policy","namespace":"loom-mills","uid":"policy-uid","resourceVersion":"101"},
		"data":{"policy.yaml":"global_enabled: false\nworkflows_enabled: true\nsubstrate_k8s_only: true\n"}
	}`
	identity, policy, err := parsePolicyConfigMap(raw, "loom-mills")
	if err != nil {
		t.Fatalf("parsePolicyConfigMap() error = %v", err)
	}
	if identity.UID != "policy-uid" || identity.ResourceVersion != "101" ||
		!strings.Contains(policy, "workflows_enabled: true") {
		t.Fatalf("parsePolicyConfigMap() = %+v %q", identity, policy)
	}
}

func TestParsePolicyConfigMapFailsClosedOnObjectIdentity(t *testing.T) {
	base := `{"metadata":{"name":"loom-mills-policy","namespace":"loom-mills","uid":"policy-uid","resourceVersion":"101"},"data":{"policy.yaml":"workflows_enabled: true"}}`
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"terminating", strings.Replace(base, `"resourceVersion":"101"`, `"resourceVersion":"101","deletionTimestamp":"2026-07-14T12:00:00Z"`, 1), "terminating"},
		{"wrong name", strings.Replace(base, "loom-mills-policy", "other-policy", 1), "name"},
		{"wrong namespace", strings.Replace(base, `"namespace":"loom-mills"`, `"namespace":"other"`, 1), "namespace"},
		{"missing resource version", strings.Replace(base, `,"resourceVersion":"101"`, "", 1), "resourceVersion"},
		{"missing policy key", strings.Replace(base, `"policy.yaml"`, `"other"`, 1), "policy.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parsePolicyConfigMap(tt.raw, "loom-mills")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parsePolicyConfigMap() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func configMapIdentityPreflight() PreflightReport {
	return PreflightReport{
		PolicyConfigMapIdentity: KubernetesObjectIdentity{
			Name: policyConfigMapName, Namespace: s1cOperatorNamespace,
			UID: "policy-uid", ResourceVersion: "10",
		},
		SpawnConfigMapUID: "spawn-uid",
		SpawnConfigMapIdentity: KubernetesObjectIdentity{
			Name: spawnStateConfigMapName, Namespace: s1cSpawnNamespace,
			UID: "spawn-uid", ResourceVersion: "20",
		},
	}
}

func TestValidateConfigMapGateIdentityFreezesCorrectBoundaries(t *testing.T) {
	initial := configMapIdentityPreflight()
	spawnAdvanced := initial
	spawnAdvanced.SpawnConfigMapIdentity.ResourceVersion = "21"
	if err := ValidateConfigMapGateIdentity(initial, spawnAdvanced); err != nil {
		t.Fatalf("spawn resourceVersion advancement rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*PreflightReport)
		want   string
	}{
		{"policy resource version", func(report *PreflightReport) { report.PolicyConfigMapIdentity.ResourceVersion = "11" }, "policy"},
		{"policy terminating", func(report *PreflightReport) { report.PolicyConfigMapIdentity.Terminating = true }, "terminating"},
		{"spawn uid", func(report *PreflightReport) {
			report.SpawnConfigMapUID = "spawn-replacement"
			report.SpawnConfigMapIdentity.UID = "spawn-replacement"
		}, "stable identity"},
		{"legacy uid mismatch", func(report *PreflightReport) { report.SpawnConfigMapUID = "forged" }, "legacy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observed := initial
			tt.mutate(&observed)
			err := ValidateConfigMapGateIdentity(initial, observed)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateConfigMapGateIdentity() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestEvaluateRejectsTerminatingConfigMapEvidence(t *testing.T) {
	evidence := passingEvidence()
	evidence.InitialPreflight.PolicyConfigMapIdentity.Terminating = true
	evidence.InitialPreflight.PolicyConfigMapIdentity.DeletionTimestamp = "2026-07-14T12:00:00Z"

	verdict := Evaluate(evidence)
	if verdict.Pass8CrashSafety {
		t.Fatal("Pass 8 must fail when the policy ConfigMap is terminating")
	}
	if verdict.Overall {
		t.Fatal("overall verdict must fail when the policy ConfigMap is terminating")
	}
	if !strings.Contains(verdict.Pass8Reason, "terminating") {
		t.Fatalf("PASS-8 reason = %q, want terminating ConfigMap failure", verdict.Pass8Reason)
	}
}

func TestEvaluateRejectsSpawnConfigMapReplacementAtCrashTarget(t *testing.T) {
	evidence := passingEvidence()
	evidence.CrashASafety.Target.SpawnState.ConfigMapUID = "replacement-spawn-uid"
	evidence.CrashASafety.Target.SpawnState.ConfigMapIdentity.UID = "replacement-spawn-uid"

	verdict := Evaluate(evidence)
	if verdict.Pass8CrashSafety {
		t.Fatal("Pass 8 must fail when the durable spawn ConfigMap is replaced")
	}
	if verdict.Overall {
		t.Fatal("overall verdict must fail when the durable spawn ConfigMap is replaced")
	}
	if !strings.Contains(verdict.Pass8Reason, "stable identity changed") {
		t.Fatalf("PASS-8 reason = %q, want stable ConfigMap identity failure", verdict.Pass8Reason)
	}
}
