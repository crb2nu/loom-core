package backend

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HasClaim reports whether a PersistentVolumeClaim exists in the sandbox
// namespace. Callers use it to verify an optional shared cache claim before
// referencing it from a pod spec: a pod that names a missing claim never
// schedules, which is a worse failure than running with a cold cache.
func (k *K8sBackend) HasClaim(ctx context.Context, name string) (bool, error) {
	_, err := k.clientset.CoreV1().PersistentVolumeClaims(k.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
