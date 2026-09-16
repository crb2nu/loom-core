package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestClonedRepoBaseSelectionScriptMapsGo126(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.test/project\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := clonedRepoBaseSelectionScript(repo)
	for _, want := range []string{
		filepath.Join(repo, "go.mod"),
		`go:1.26) DEVBOX_BASE_IMAGE="registry.harbor.lan/mcp/devbox-base/go:1.26"`,
		`DEVBOX_BASE_SELECTION`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("selection script missing %q:\n%s", want, script)
		}
	}
	out, err := exec.CommandContext(t.Context(), "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("run selection script: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "image=registry.harbor.lan/mcp/devbox-base/go:1.26 reason=") {
		t.Fatalf("selection output = %q", out)
	}
}

func TestParseBaseImageFallback(t *testing.T) {
	if got := parseBaseImageFallback("DEVBOX_BASE_SELECTION language=go version=1.26 image=x reason=\n"); got != nil {
		t.Fatalf("mapped selection parsed as fallback: %#v", got)
	}
	got := parseBaseImageFallback("noise\nDEVBOX_BASE_SELECTION language=go version=1.27 image=x reason=unmapped_version\n")
	if got == nil || got.Language != "go" || got.Version != "1.27" || got.Reason != "unmapped_version" {
		t.Fatalf("fallback = %#v", got)
	}
}

func TestBuildBuildahPodSpecPopulatesRegistryLayerCache(t *testing.T) {
	k := testK8sBackend()
	pod := k.buildBuildahPodSpec(
		"buildah-build-cache",
		"registry.test/mcp/devbox/loom-core:revision",
		"buildah-dockerfile-cache",
		"/workspace/services/loom-core",
		false,
		false,
	)

	command := pod.Spec.Containers[0].Command[2]
	for _, want := range []string{
		"--layers",
		"--cache-from=registry.test/mcp/devbox/loom-core",
		"--cache-to=registry.test/mcp/devbox/loom-core",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("build command missing %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "registry.test/mcp/devbox/loom-core:cache") {
		t.Fatalf("build command still tags or pushes the moving :cache image:\n%s", command)
	}
}

func TestK8sRunBuildPodJoinsRunningBuild(t *testing.T) {
	podName := "buildah-build-shared"
	clientset := k8sfake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "devbox"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
	k := testK8sBackend()
	k.clientset = clientset

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			for _, action := range clientset.Actions() {
				if action.GetVerb() == "watch" {
					pod, _ := clientset.CoreV1().Pods("devbox").Get(context.Background(), podName, metav1.GetOptions{})
					pod.Status.Phase = corev1.PodSucceeded
					_, _ = clientset.CoreV1().Pods("devbox").Update(context.Background(), pod, metav1.UpdateOptions{})
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	result, err := k.runBuildPod(context.Background(), podName, "registry.test/repo:image", "cm", "/workspace", false, false)
	if err != nil {
		t.Fatalf("runBuildPod: %v", err)
	}
	<-done
	if result.ImageTag != "registry.test/repo:image" {
		t.Fatalf("ImageTag = %q", result.ImageTag)
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "delete" || action.GetVerb() == "create" {
			t.Fatalf("joined active pod received unexpected %s action", action.GetVerb())
		}
	}
}

func TestK8sCreateDockerfileConfigMapReusesConcurrentBuildInput(t *testing.T) {
	const name = "buildah-dockerfile-shared"
	clientset := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devbox"},
		Data:       map[string]string{"Dockerfile": "same immutable input"},
	})
	k := testK8sBackend()
	k.clientset = clientset

	if err := k.createDockerfileConfigMap(context.Background(), name, []byte("same immutable input")); err != nil {
		t.Fatalf("createDockerfileConfigMap: %v", err)
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("same-tag build input was deleted before joining its active pod")
		}
	}
}

func TestK8sRunBuildPodReplacesTerminalBuild(t *testing.T) {
	podName := "buildah-build-terminal"
	clientset := k8sfake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "devbox"}, Status: corev1.PodStatus{Phase: corev1.PodFailed}})
	clientset.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		create := action.(ktesting.CreateAction)
		create.GetObject().(*corev1.Pod).Status.Phase = corev1.PodSucceeded
		return false, nil, nil
	})
	k := testK8sBackend()
	k.clientset = clientset
	if _, err := k.runBuildPod(context.Background(), podName, "registry.test/repo:image", "cm", "/workspace", false, false); err != nil {
		t.Fatalf("runBuildPod: %v", err)
	}
	var deleted, created bool
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "delete" {
			deleted = true
		}
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			created = true
		}
	}
	if !deleted || !created {
		t.Fatalf("terminal replacement actions: deleted=%v created=%v", deleted, created)
	}
}

func TestK8sRunBuildPodPreferExistingRegistryHit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || !strings.Contains(r.URL.Path, "/v2/team/image/manifests/tag") {
			t.Errorf("unexpected probe %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	k := testK8sBackend()
	k.clientset = k8sfake.NewSimpleClientset()
	image := strings.TrimPrefix(server.URL, "http://") + "/team/image:tag"
	result, err := k.runBuildPod(context.Background(), "buildah-build-hit", image, "cm", "/workspace", true, false)
	if err != nil {
		t.Fatalf("runBuildPod: %v", err)
	}
	if !result.Cached {
		t.Fatal("registry hit should return Cached=true")
	}
	for _, action := range k.clientset.(*k8sfake.Clientset).Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			t.Fatal("registry hit created a build pod")
		}
	}
}

func TestCleanupBuilds_DeletesOldCompletedPods(t *testing.T) {
	oldTime := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	recentTime := metav1.NewTime(time.Now().Add(-30 * time.Minute))

	clientset := k8sfake.NewSimpleClientset(
		// Old succeeded pod — should be deleted
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "buildah-build-old-succeeded",
				Namespace:         "devbox",
				Labels:            map[string]string{"devbox/build": "buildah"},
				CreationTimestamp: oldTime,
			},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
		// Old failed pod — should be deleted
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "buildah-build-old-failed",
				Namespace:         "devbox",
				Labels:            map[string]string{"devbox/build": "buildah"},
				CreationTimestamp: oldTime,
			},
			Status: corev1.PodStatus{Phase: corev1.PodFailed},
		},
		// Recent succeeded pod — should NOT be deleted
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "buildah-build-recent",
				Namespace:         "devbox",
				Labels:            map[string]string{"devbox/build": "buildah"},
				CreationTimestamp: recentTime,
			},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
		// Old running pod — should NOT be deleted
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "buildah-build-running",
				Namespace:         "devbox",
				Labels:            map[string]string{"devbox/build": "buildah"},
				CreationTimestamp: oldTime,
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		// Associated ConfigMap for old-succeeded (has build label)
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "buildah-dockerfile-old-succeeded",
				Namespace:         "devbox",
				Labels:            map[string]string{"devbox/build": "buildah"},
				CreationTimestamp: oldTime,
			},
		},
		// Orphaned ConfigMap — pod already deleted, should still be cleaned by label
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "buildah-dockerfile-orphaned",
				Namespace:         "devbox",
				Labels:            map[string]string{"devbox/build": "buildah"},
				CreationTimestamp: oldTime,
			},
		},
		// Recent ConfigMap — should NOT be deleted
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "buildah-dockerfile-recent",
				Namespace:         "devbox",
				Labels:            map[string]string{"devbox/build": "buildah"},
				CreationTimestamp: recentTime,
			},
		},
	)

	k := testK8sBackend()
	k.clientset = clientset

	cleaned, err := k.CleanupBuilds(context.Background(), 1*time.Hour)
	if err != nil {
		t.Fatalf("CleanupBuilds error: %v", err)
	}

	// 2 pods deleted (old-succeeded + old-failed)
	if cleaned != 2 {
		t.Errorf("expected 2 cleaned pods, got %d", cleaned)
	}

	// Verify remaining pods
	pods, _ := clientset.CoreV1().Pods("devbox").List(context.Background(), metav1.ListOptions{})
	remaining := make(map[string]bool)
	for _, p := range pods.Items {
		remaining[p.Name] = true
	}
	if remaining["buildah-build-old-succeeded"] {
		t.Error("old succeeded pod should have been deleted")
	}
	if remaining["buildah-build-old-failed"] {
		t.Error("old failed pod should have been deleted")
	}
	if !remaining["buildah-build-recent"] {
		t.Error("recent pod should NOT have been deleted")
	}
	if !remaining["buildah-build-running"] {
		t.Error("running pod should NOT have been deleted")
	}

	// Verify ConfigMap cleanup (label-based second pass)
	cms, _ := clientset.CoreV1().ConfigMaps("devbox").List(context.Background(), metav1.ListOptions{})
	remainingCMs := make(map[string]bool)
	for _, cm := range cms.Items {
		remainingCMs[cm.Name] = true
	}
	if remainingCMs["buildah-dockerfile-old-succeeded"] {
		t.Error("old ConfigMap should have been deleted")
	}
	if remainingCMs["buildah-dockerfile-orphaned"] {
		t.Error("orphaned ConfigMap should have been deleted by label scan")
	}
	if !remainingCMs["buildah-dockerfile-recent"] {
		t.Error("recent ConfigMap should NOT have been deleted")
	}
}

func TestReadDepFiles_Empty(t *testing.T) {
	dir := t.TempDir()
	files := readDepFiles(dir)
	if len(files) != 0 {
		t.Errorf("expected empty map for empty dir, got %d files", len(files))
	}
}

func TestReadDepFiles_GoProject(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n\ngo 1.25"), 0644)
	os.WriteFile(filepath.Join(dir, "go.sum"), []byte("hash1\nhash2"), 0644)

	files := readDepFiles(dir)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if _, ok := files["go.mod"]; !ok {
		t.Error("expected go.mod in result")
	}
	if _, ok := files["go.sum"]; !ok {
		t.Error("expected go.sum in result")
	}
}

func TestReadDepFiles_NodeProject(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0644)
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte("lockfile"), 0644)

	files := readDepFiles(dir)
	if _, ok := files["package.json"]; !ok {
		t.Error("expected package.json")
	}
	if _, ok := files["pnpm-lock.yaml"]; !ok {
		t.Error("expected pnpm-lock.yaml")
	}
}

func TestReadDepFiles_MixedProject(t *testing.T) {
	dir := t.TempDir()
	// Simulate a project with both Go and Python deps.
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)
	os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]"), 0644)
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask==3.0"), 0644)

	files := readDepFiles(dir)
	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d: %v", len(files), files)
	}
}

func TestReadDepFiles_NonExistentDir(t *testing.T) {
	files := readDepFiles("/nonexistent/path/for/testing")
	if len(files) != 0 {
		t.Errorf("expected empty map for nonexistent dir, got %d", len(files))
	}
}

func TestReadDepFiles_IgnoresNonDepFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0644)

	files := readDepFiles(dir)
	if len(files) != 0 {
		t.Errorf("expected 0 dep files, got %d: %v", len(files), files)
	}
}

// Virtual time makes the queue longer than the configured budget without a
// slow test. Cover both ownership paths and budgets beyond the old 35m cap.
func TestBuildBudgetExcludesQueueAndSetup(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for _, budget := range []time.Duration{time.Minute, 90 * time.Minute} {
			t.Run(fmt.Sprintf("joined=%v/budget=%s", joined, budget), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					k := testK8sBackend()
					k.buildTimeout = budget
					k.buildSlots = make(chan struct{}, 1)
					k.buildSlots <- struct{}{}
					podName := "buildah-build-budget"
					pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: k.namespace}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
					client := k8sfake.NewSimpleClientset()
					if joined {
						client = k8sfake.NewSimpleClientset(pod)
					}
					k.clientset = client
					client.PrependReactor("create", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
						time.Sleep(4 * time.Minute)
						return false, nil, nil
					})
					client.PrependReactor("create", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
						time.Sleep(4 * time.Minute)
						return false, nil, nil
					})
					watcher := watch.NewRaceFreeFake()
					defer watcher.Stop()
					watching := make(chan struct{})
					client.PrependWatchReactor("pods", func(ktesting.Action) (bool, watch.Interface, error) {
						close(watching)
						return true, watcher, nil
					})
					done := make(chan error, 1)
					go func() {
						_, err := k.Build(context.Background(), BuildOpts{Tag: "budget", ContextDir: k.workspaceRoot, Dockerfile: []byte("FROM scratch")})
						done <- err
					}()
					synctest.Wait()
					time.Sleep(2 * budget)
					if len(client.Actions()) != 0 {
						t.Fatal("build touched Kubernetes while queued")
					}
					<-k.buildSlots
					<-watching
					time.Sleep(budget * 3 / 4)
					synctest.Wait()
					select {
					case err := <-done:
						t.Fatalf("build ended before full pod budget: %v", err)
					default:
					}
					completed := pod.DeepCopy()
					completed.Status.Phase = corev1.PodSucceeded
					watcher.Modify(completed)
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					if len(k.buildSlots) != 0 {
						t.Fatal("build slot was not released")
					}
				})
			})
		}
	}
}

func TestBuildTimeoutAndSlotRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := testK8sBackend()
		client := k8sfake.NewSimpleClientset()
		client.PrependWatchReactor("pods", func(ktesting.Action) (bool, watch.Interface, error) {
			return true, closedFakeWatcher(), nil
		})
		k.clientset = client
		k.buildTimeout = time.Minute
		k.buildSlots = make(chan struct{}, 1)
		start := time.Now()
		_, err := k.Build(context.Background(), BuildOpts{Tag: "timeout", ContextDir: k.workspaceRoot})
		if err == nil || !strings.Contains(err.Error(), "timed out after 1m0s waiting for pod") {
			t.Fatalf("got %v", err)
		}
		if elapsed := time.Since(start); elapsed < time.Minute || elapsed > 3*time.Minute {
			t.Fatalf("unexpected elapsed time: %s", elapsed)
		}
		if len(k.buildSlots) != 0 {
			t.Fatal("timed-out build leaked slot")
		}
	})
}

func TestBuildQueuedCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := testK8sBackend()
		k.clientset = k8sfake.NewSimpleClientset()
		k.buildSlots = make(chan struct{}, 1)
		k.buildSlots <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := k.Build(ctx, BuildOpts{}); done <- err }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		if len(k.buildSlots) != 1 {
			t.Fatal("canceled waiter released another build's slot")
		}
		if len(k.clientset.(*k8sfake.Clientset).Actions()) != 0 {
			t.Fatal("canceled waiter touched Kubernetes")
		}
	})
}

func TestJoinedBuildUsesConfiguredTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := testK8sBackend()
		k.buildTimeout = time.Minute
		k.clientset = k8sfake.NewSimpleClientset(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "joined", Namespace: k.namespace},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
		k.clientset.(*k8sfake.Clientset).PrependWatchReactor("pods", func(ktesting.Action) (bool, watch.Interface, error) {
			return true, closedFakeWatcher(), nil
		})
		start := time.Now()
		_, err := k.runBuildPod(context.Background(), "joined", "image", "cm", "/workspace", false, false)
		if err == nil || !strings.Contains(err.Error(), "timed out after 1m0s") {
			t.Fatalf("got %v", err)
		}
		if elapsed := time.Since(start); elapsed != time.Minute {
			t.Fatalf("elapsed %s", elapsed)
		}
		for _, action := range k.clientset.(*k8sfake.Clientset).Actions() {
			if action.GetVerb() == "delete" {
				t.Fatal("joiner deleted another caller's pod")
			}
		}
	})
}
