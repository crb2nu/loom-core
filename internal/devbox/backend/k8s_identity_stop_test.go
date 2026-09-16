package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestK8sStopIfIdentityConfirmsExactPodTermination(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "still terminating", true: "replacement"}[replacement], func(t *testing.T) {
			k := testK8sBackend()
			labels := map[string]string{"loom.dev/spawn-id": "owned-spawn"}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: k.namespace, UID: "old", Labels: labels}}
			client := k8sfake.NewSimpleClientset(pod)
			k.clientset = client
			deletes, getsAfterDelete := 0, 0
			client.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				deletes++
				opts := action.(k8stesting.DeleteAction).GetDeleteOptions()
				if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != pod.UID {
					t.Error("missing UID fence")
				}
				return true, nil, nil // Accepted, but the old worker can still run.
			})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if deletes > 0 {
					getsAfterDelete++
					if replacement {
						return true, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, UID: "new"}}, nil
					}
					// Cancel deterministically only after the termination check.
					cancel()
				}
				return false, nil, nil
			})
			err := k.StopIfIdentity(ctx, pod.Name, labels)
			if !replacement && !errors.Is(err, context.Canceled) {
				t.Fatalf("acknowledged an unterminated worker: %v", err)
			}
			if replacement && err != nil {
				t.Fatal(err)
			}
			if deletes != 1 || getsAfterDelete == 0 {
				t.Fatalf("must wait for exactly the deleted UID: deletes=%d checks=%d", deletes, getsAfterDelete)
			}
		})
	}
}
