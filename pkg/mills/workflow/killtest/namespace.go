package killtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type namespaceListWire struct {
	Items []struct {
		Metadata struct {
			Name              string  `json:"name"`
			UID               string  `json:"uid"`
			ResourceVersion   string  `json:"resourceVersion"`
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

// validateActiveNamespaces proves that every configured namespace exists as a
// distinct, live Kubernetes object. A successful name-only kubectl lookup is
// insufficient because terminating namespaces remain readable while their
// workloads and policy objects are being deleted.
func validateActiveNamespaces(raw string, expected ...string) error {
	if len(expected) == 0 {
		return errors.New("no namespaces configured")
	}
	wanted := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("configured namespace name is empty")
		}
		if _, duplicate := wanted[name]; duplicate {
			return fmt.Errorf("configured namespace %q is duplicated", name)
		}
		wanted[name] = struct{}{}
	}

	var list namespaceListWire
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return fmt.Errorf("decode namespace list: %w", err)
	}
	seen := make(map[string]struct{}, len(list.Items))
	for _, namespace := range list.Items {
		name := strings.TrimSpace(namespace.Metadata.Name)
		if _, required := wanted[name]; !required {
			return fmt.Errorf("namespace lookup returned unexpected object %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("namespace lookup returned duplicate object %q", name)
		}
		seen[name] = struct{}{}
		if strings.TrimSpace(namespace.Metadata.UID) == "" ||
			strings.TrimSpace(namespace.Metadata.ResourceVersion) == "" {
			return fmt.Errorf("namespace %q has incomplete object identity", name)
		}
		if namespace.Metadata.DeletionTimestamp != nil || namespace.Status.Phase != "Active" {
			return fmt.Errorf("namespace %q is not active: phase=%q deletion_timestamp=%v",
				name, namespace.Status.Phase, namespace.Metadata.DeletionTimestamp)
		}
	}
	for name := range wanted {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("namespace lookup omitted required object %q", name)
		}
	}
	return nil
}
