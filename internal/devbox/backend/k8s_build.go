package backend

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/devbox/baseimage"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// buildDepFiles are dependency manifest files included in the build ConfigMap
// for tar-pipe mode. These are the files that Dockerfile templates COPY.
var buildDepFiles = []string{
	"go.mod", "go.sum",
	"package.json", "pnpm-lock.yaml", "yarn.lock", "package-lock.json",
	"pyproject.toml", "uv.lock", "poetry.lock", "requirements.txt",
	"Cargo.toml", "Cargo.lock",
	"Gemfile", "Gemfile.lock",
}

const (
	buildMaxRetries = 2
	// podEvictTimeout caps how long evictPod waits for a terminating
	// pod to disappear before recreating. 30s comfortably covers the
	// typical 1-5s termination window seen in production. Tuned to be
	// well under the outer buildMaxRetries backoff so a stubborn pod
	// gets a second eviction attempt without blowing the build budget.
	podEvictTimeout = 30 * time.Second

	// DefaultBuildTimeout is the per-pod build budget, excluding queue and setup.
	DefaultBuildTimeout = 30 * time.Minute

	// Setup and diagnostics have their own bounds so they cannot consume a pod's budget.
	buildSetupTimeout = 5 * time.Minute
)

func (k *K8sBackend) Build(ctx context.Context, opts BuildOpts) (*BuildResult, error) {
	queuedAt := time.Now()
	release, err := k.acquireBuildSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	slog.Info("build slot acquired", "tag", opts.Tag, "queue_wait", time.Since(queuedAt), "build_timeout", k.effectiveBuildTimeout())

	registryTag := k.registryTag(opts.Tag)

	// Compute NFS-relative paths so the Buildah pod can find files via the shared PVC.
	contextRel, err := filepath.Rel(k.workspaceRoot, opts.ContextDir)
	if err != nil || strings.HasPrefix(contextRel, "..") {
		return nil, fmt.Errorf("context dir %q is not under workspace root %q", opts.ContextDir, k.workspaceRoot)
	}

	// Detach from the request context: builds are long-running and must
	// survive MCP proxy timeouts / client disconnects. Bound setup separately
	// from each pod wait so neither setup nor retries truncate the pod budget.
	buildCtx := context.Background()
	setupCtx, cancel := context.WithTimeout(buildCtx, buildSetupTimeout)
	defer cancel()

	buildName := sanitizeBuildName(opts.Tag)

	// Inject Dockerfile (and dep files in tar-pipe mode) via ConfigMap.
	cmName := "buildah-dockerfile-" + buildName

	// In tar-pipe mode, bundle dep files into the ConfigMap and use it as build context.
	// This eliminates the need for workspace volume during builds.
	buildContextDir := "/workspace/" + contextRel
	if k.syncMode == "tar-pipe" {
		depFiles := readDepFiles(opts.ContextDir)
		if err := k.createBuildConfigMap(setupCtx, cmName, opts.Dockerfile, depFiles); err != nil {
			return nil, fmt.Errorf("create build configmap: %w", err)
		}
		buildContextDir = "/buildah-dockerfile"
	} else {
		if err := k.createDockerfileConfigMap(setupCtx, cmName, opts.Dockerfile); err != nil {
			return nil, fmt.Errorf("create dockerfile configmap: %w", err)
		}
	}
	defer func() {
		_ = k.deleteConfigMap(context.Background(), cmName)
	}()

	podName := "buildah-build-" + buildName

	var lastErr error
	for attempt := range buildMaxRetries {
		result, err := k.runBuildPod(buildCtx, podName, registryTag, cmName, buildContextDir, opts.PreferExisting, opts.DetectBaseImage)
		if err == nil {
			return result, nil
		}
		lastErr = err

		if attempt < buildMaxRetries-1 {
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
	}
	return nil, lastErr
}

// runBuildPod creates a Buildah build pod, waits for completion, and returns the result.
// buildContext is the absolute path inside the pod (e.g., "/workspace/services/loom-core"
// or "/buildah-dockerfile" for tar-pipe mode).
func (k *K8sBackend) runBuildPod(ctx context.Context, podName, registryTag, cmName, buildContext string, preferExisting, detectBaseImage bool) (*BuildResult, error) {
	setupCtx, cancel := context.WithTimeout(ctx, buildSetupTimeout)
	defer cancel()
	// A clone-time selection build must inspect the hydrated repository. An
	// existing empty-fingerprint image may have been built by the legacy static
	// Go 1.25 path, so it is not a safe cache hit.
	if preferExisting && !detectBaseImage && k.registryImageExists(setupCtx, registryTag) {
		return &BuildResult{ImageTag: registryTag, Cached: true}, nil
	}

	terminalLeftover := false
	if existing, err := k.clientset.CoreV1().Pods(k.namespace).Get(setupCtx, podName, metav1.GetOptions{}); err == nil {
		if existing.Status.Phase == corev1.PodPending || existing.Status.Phase == corev1.PodRunning {
			return k.joinBuildPod(ctx, podName, registryTag)
		}
		terminalLeftover = true
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get existing buildah pod: %w", err)
	}

	pod := k.buildBuildahPodSpec(podName, registryTag, cmName, buildContext, preferExisting, detectBaseImage)

	// Evict any leftover build pod with the same name AND wait for the
	// terminating pod to actually disappear. The previous code called
	// deletePod (fire-and-forget) and immediately tried to Create — a
	// terminating pod still occupies the name and the Create returned
	// 409 AlreadyExists. Per the 2026-05-24 kill-test that pattern
	// accounted for ~20% of buildah build failures.
	// Don't fail outright on eviction timeout — the Create call below
	// will surface a 409 with the actual error context if the pod is
	// genuinely still there, and the next retry loop handles that.
	if terminalLeftover {
		_ = k.evictPod(setupCtx, podName, podEvictTimeout)
	}

	if _, err := k.clientset.CoreV1().Pods(k.namespace).Create(setupCtx, pod, metav1.CreateOptions{}); err != nil {
		// A concurrent caller may have created the deterministic pod after
		// our initial Get. Join it when active; never evict an in-flight build.
		if apierrors.IsAlreadyExists(err) {
			existing, getErr := k.clientset.CoreV1().Pods(k.namespace).Get(setupCtx, podName, metav1.GetOptions{})
			if getErr == nil && (existing.Status.Phase == corev1.PodPending || existing.Status.Phase == corev1.PodRunning) {
				return k.joinBuildPod(ctx, podName, registryTag)
			}
			return nil, fmt.Errorf("create buildah pod: %w", err)
		} else {
			return nil, fmt.Errorf("create buildah pod: %w", err)
		}
	}
	defer func() {
		_ = k.deletePod(context.Background(), podName)
	}()

	// Start the full pod budget only after creation.
	waitErr := k.waitForPodDone(ctx, podName, k.effectiveBuildTimeout())
	ctx, cancelLogs := context.WithTimeout(ctx, time.Minute)
	defer cancelLogs()
	if err := waitErr; err != nil {
		// A failed git-clone INIT container (repo not found / bad ref / auth)
		// terminates before buildah runs, so getPodLogs (the buildah container)
		// is empty and the error is the opaque "container git-clone terminated
		// exit_code=128 reason=Error". Capture the init container's real
		// `fatal: …` git line so the failure is actionable and classifiable;
		// fall back to the buildah container tail for genuine build failures.
		if detail := k.failedInitContainerDetail(ctx, podName); detail != "" {
			return nil, fmt.Errorf("buildah build failed: %w%s", err, detail)
		}
		logs, _ := k.getPodLogs(ctx, podName)
		return nil, fmt.Errorf("buildah build failed: %w\n%s", err, logs)
	}

	// Read build logs and check for cache hits
	logs, _ := k.getPodLogs(ctx, podName)
	cached := strings.Contains(logs, "Using cache") ||
		strings.Contains(logs, "--> Using cache") ||
		strings.Contains(logs, "Using existing image")

	return &BuildResult{ImageTag: registryTag, Cached: cached, BaseImageFallback: parseBaseImageFallback(logs)}, nil
}

func (k *K8sBackend) joinBuildPod(ctx context.Context, podName, registryTag string) (*BuildResult, error) {
	if err := k.waitForPodDone(ctx, podName, k.effectiveBuildTimeout()); err != nil {
		return nil, fmt.Errorf("buildah build failed: %w", err)
	}
	return &BuildResult{ImageTag: registryTag}, nil
}

var bearerChallengeRE = regexp.MustCompile(`(?i)^Bearer\s+realm="([^"]+)"(?:,service="([^"]*)")?(?:,scope="([^"]*)")?`)

// registryImageExists probes the destination registry with the same pull
// credentials mounted into Buildah. Probe failures are conservative misses.
func (k *K8sBackend) registryImageExists(ctx context.Context, image string) bool {
	host, repo, tag, ok := splitRegistryImage(image)
	if !ok {
		return false
	}
	scheme := "https"
	if strings.HasPrefix(host, "localhost:") || strings.HasPrefix(host, "127.0.0.1:") {
		scheme = "http"
	}
	manifestURL := scheme + "://" + host + "/v2/" + repo + "/manifests/" + url.PathEscape(tag)
	username, password := k.RegistryCredentials(ctx, host)
	client, err := RegistryClient(10 * time.Second)
	if err != nil {
		return false
	}
	request := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else if username != "" {
			req.SetBasicAuth(username, password)
		}
		return client.Do(req)
	}
	resp, err := request("")
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}
	match := bearerChallengeRE.FindStringSubmatch(resp.Header.Get("WWW-Authenticate"))
	if resp.StatusCode != http.StatusUnauthorized || len(match) == 0 {
		return false
	}
	tokenURL, err := url.Parse(match[1])
	if err != nil {
		return false
	}
	q := tokenURL.Query()
	if match[2] != "" {
		q.Set("service", match[2])
	}
	if match[3] != "" {
		q.Set("scope", match[3])
	}
	tokenURL.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	tokenResp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer tokenResp.Body.Close()
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if tokenResp.StatusCode/100 != 2 || json.NewDecoder(tokenResp.Body).Decode(&payload) != nil {
		return false
	}
	if payload.Token == "" {
		payload.Token = payload.AccessToken
	}
	resp, err = request(payload.Token)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func splitRegistryImage(image string) (host, repo, tag string, ok bool) {
	image = strings.TrimPrefix(strings.TrimPrefix(image, "https://"), "http://")
	slash := strings.IndexByte(image, '/')
	colon := strings.LastIndex(image, ":")
	if slash <= 0 || colon <= slash+1 {
		return "", "", "", false
	}
	return image[:slash], image[slash+1 : colon], image[colon+1:], true
}

// RegistryCredentials reads credentials for host from the Buildah image pull secret.
func (k *K8sBackend) RegistryCredentials(ctx context.Context, host string) (string, string) {
	secret, err := k.clientset.CoreV1().Secrets(k.namespace).Get(ctx, k.imagePullSecret, metav1.GetOptions{})
	if err != nil {
		return "", ""
	}
	var cfg struct {
		Auths map[string]struct{ Auth, Username, Password string } `json:"auths"`
	}
	if json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &cfg) != nil {
		return "", ""
	}
	for registry, auth := range cfg.Auths {
		if strings.TrimPrefix(strings.TrimPrefix(registry, "https://"), "http://") != host {
			continue
		}
		if auth.Username != "" {
			return auth.Username, auth.Password
		}
		decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
		if err == nil {
			if i := strings.IndexByte(string(decoded), ':'); i >= 0 {
				return string(decoded[:i]), string(decoded[i+1:])
			}
		}
	}
	return "", ""
}

// RegistryClient returns the HTTP client shared by build cache and startup probes.
// Without an explicit CA it mirrors Buildah's tls-verify=false posture.
func RegistryClient(timeout time.Duration) (*http.Client, error) {
	tlsConfig := &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- parity with Buildah's explicit tls-verify=false.
	if caFile := strings.TrimSpace(os.Getenv("DEVBOX_REGISTRY_CA_FILE")); caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read registry CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("registry CA file contains no certificates")
		}
		tlsConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: timeout}, nil
}

// buildBuildahPodSpec creates a Pod spec for a Buildah in-cluster build.
// buildContext is the absolute path inside the pod to use as the Docker build context
// (e.g., "/workspace/services/loom-core" for NFS/git, "/buildah-dockerfile" for tar-pipe).
func (k *K8sBackend) buildBuildahPodSpec(podName, destination, dockerfileCM, buildContext string, preferExisting, detectBaseImage bool) *corev1.Pod {
	gracePeriod := int64(0)
	runAsUser := int64(0)
	runAsGroup := int64(0)

	// Cache repo: repository path without tag for --cache-from and --cache-to (buildah v1.29+
	// requires a bare repository reference, no tag or digest).
	cacheRepo := destination
	if idx := strings.LastIndex(cacheRepo, ":"); idx > 0 {
		cacheRepo = cacheRepo[:idx]
	}

	buildSteps := []string{
		// Configure registries for short-name resolution (non-interactive builds)
		"mkdir -p /etc/containers",
		"&&",
		`printf 'unqualified-search-registries = ["docker.io"]\nshort-name-mode = "permissive"\n' > /etc/containers/registries.conf`,
		"&&",
	}
	if preferExisting && !detectBaseImage {
		buildSteps = append(buildSteps,
			"if command -v skopeo >/dev/null 2>&1 && skopeo inspect --raw --tls-verify=false docker://"+destination+" >/dev/null 2>&1; then",
			"echo Using existing image "+destination+";",
			"exit 0;",
			"fi",
			"&&",
			"manifest_err=$(mktemp);",
			"if buildah manifest inspect --tls-verify=false "+destination+" >/dev/null 2>\"$manifest_err\"; then",
			"echo Using existing image "+destination+";",
			"exit 0;",
			"fi;",
			"if grep -q 'not a list type' \"$manifest_err\"; then",
			"echo Using existing image "+destination+";",
			"exit 0;",
			"fi;",
			"cat \"$manifest_err\" >&2",
			"&&",
		)
	}
	if detectBaseImage {
		buildSteps = append(buildSteps, clonedRepoBaseSelectionScript(buildContext), "&&")
	}
	buildArgs := ""
	if detectBaseImage {
		buildArgs = "--build-arg DEVBOX_BASE_IMAGE=\"$DEVBOX_BASE_IMAGE\" "
	}
	buildSteps = append(buildSteps,
		"buildah build-using-dockerfile",
		"--storage-driver=vfs",
		"--isolation=chroot",
		"--tls-verify=false",
		"--layers", buildArgs,
		"--cache-from="+cacheRepo,
		"--cache-to="+cacheRepo,
		"-f /buildah-dockerfile/Dockerfile",
		"-t "+destination,
		buildContext,
		"&&",
		"buildah push --storage-driver=vfs --tls-verify=false "+destination,
	)
	buildAndPush := strings.Join(buildSteps, " ")

	workspaceSizeLimit := resource.MustParse("5Gi")
	workspace := k.workspacePlan(buildContext, gitCloneOpts{}, &workspaceSizeLimit)
	volumeMounts := append([]corev1.VolumeMount{}, workspace.volumeMounts...)
	volumeMounts = append(volumeMounts,
		corev1.VolumeMount{Name: "dockerfile", MountPath: "/buildah-dockerfile", ReadOnly: true},
		corev1.VolumeMount{Name: "buildah-storage", MountPath: "/var/lib/containers/storage"},
		corev1.VolumeMount{Name: "auth", MountPath: "/run/containers/0/auth.json", SubPath: "config.json", ReadOnly: true},
	)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: k.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "mcp-devbox",
				"devbox/build":                 "buildah",
			},
			Annotations: map[string]string{
				// Belt-and-suspenders: deprecated annotation for pre-1.30 clusters
				"container.apparmor.security.beta.kubernetes.io/buildah": "unconfined",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ServiceAccountName:            "mcp-devbox",
			TerminationGracePeriodSeconds: &gracePeriod,
			NodeSelector: map[string]string{
				"kubernetes.io/arch": "amd64",
			},
			Affinity:       avoidNodesAffinity(k.buildAvoidNodes),
			InitContainers: workspace.initContainers,
			Containers: []corev1.Container{
				{
					Name:    "buildah",
					Image:   k.builderImage,
					Command: []string{"sh", "-c", buildAndPush},
					Env: []corev1.EnvVar{
						{Name: "BUILDAH_ISOLATION", Value: "chroot"},
						{Name: "STORAGE_DRIVER", Value: "vfs"},
						{Name: "CONTAINERS_REGISTRIES_CONF", Value: "/etc/containers/registries.conf"},
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: boolPtr(true),
						RunAsUser:  &runAsUser,
						RunAsGroup: &runAsGroup,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:              k.buildCPURequest,
							corev1.ResourceMemory:           k.buildMemoryRequest,
							corev1.ResourceEphemeralStorage: k.buildEphemeralStorageRequest,
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:              k.buildCPULimit,
							corev1.ResourceMemory:           k.buildMemoryLimit,
							corev1.ResourceEphemeralStorage: k.buildEphemeralStorageLimit,
						},
					},
					VolumeMounts: volumeMounts,
				},
			},
			Volumes: []corev1.Volume{
				workspace.volume,
				{
					Name: "dockerfile",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: dockerfileCM,
							},
						},
					},
				},
				{
					Name: "buildah-storage",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: resourcePtr(resource.MustParse("40Gi")),
						},
					},
				},
				{
					Name: "auth",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: k.imagePullSecret,
							Items: []corev1.KeyToPath{
								{Key: ".dockerconfigjson", Path: "config.json"},
							},
						},
					},
				},
			},
		},
	}
}

func clonedRepoBaseSelectionScript(buildContext string) string {
	var cases strings.Builder
	for _, item := range baseimage.Languages() {
		fmt.Fprintf(&cases, "%s:%s) DEVBOX_BASE_IMAGE=%q ;; ", item.Language, item.Version, baseimage.Lookup(item.Language, item.Version))
	}
	goMod := shellQuote(filepath.Join(buildContext, "go.mod"))
	return fmt.Sprintf(`language=unknown; version=unknown; reason=no_language_detected; DEVBOX_BASE_IMAGE=registry.harbor.lan/mcp/devbox-base/go:1.25; if [ -f %s ]; then language=go; version=$(awk '$1=="go" {split($2,v,"."); print v[1] "." v[2]; exit}' %s); reason=unmapped_version; fi; case "$language:$version" in %s*) ;; esac; if [ "$DEVBOX_BASE_IMAGE" = registry.harbor.lan/mcp/devbox-base/go:1.25 ] && [ "$language:$version" = "go:1.25" ]; then reason=; elif [ "$DEVBOX_BASE_IMAGE" != registry.harbor.lan/mcp/devbox-base/go:1.25 ]; then reason=; fi; export DEVBOX_BASE_IMAGE; printf 'DEVBOX_BASE_SELECTION language=%%s version=%%s image=%%s reason=%%s\n' "$language" "$version" "$DEVBOX_BASE_IMAGE" "$reason"`, goMod, goMod, cases.String())
}

func parseBaseImageFallback(logs string) *BaseImageFallback {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.HasPrefix(line, "DEVBOX_BASE_SELECTION ") {
			continue
		}
		fields := map[string]string{}
		for _, field := range strings.Fields(strings.TrimPrefix(line, "DEVBOX_BASE_SELECTION ")) {
			parts := strings.SplitN(field, "=", 2)
			if len(parts) == 2 {
				fields[parts[0]] = parts[1]
			}
		}
		if fields["reason"] != "" {
			return &BaseImageFallback{Language: fields["language"], Version: fields["version"], Reason: fields["reason"]}
		}
	}
	return nil
}

func avoidNodesAffinity(nodes []string) *corev1.Affinity {
	if len(nodes) == 0 {
		return nil
	}
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "kubernetes.io/hostname",
						Operator: corev1.NodeSelectorOpNotIn,
						Values:   nodes,
					}},
				}},
			},
		},
	}
}

func (k *K8sBackend) acquireBuildSlot(ctx context.Context) (func(), error) {
	if k.buildSlots == nil {
		return func() {}, nil
	}
	select {
	case k.buildSlots <- struct{}{}:
		return func() { <-k.buildSlots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for build slot: %w", ctx.Err())
	}
}

// createBuildConfigMap creates a ConfigMap containing the Dockerfile and dep files.
// Used in tar-pipe mode where the ConfigMap serves as the entire build context.
func (k *K8sBackend) createBuildConfigMap(ctx context.Context, name string, dockerfile []byte, depFiles map[string]string) error {
	data := map[string]string{
		"Dockerfile": string(dockerfile),
	}
	for fname, content := range depFiles {
		data[fname] = content
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "mcp-devbox",
				"devbox/build":                 "buildah",
			},
		},
		Data: data,
	}
	_, err := k.clientset.CoreV1().ConfigMaps(k.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// ConfigMap names include the immutable image fingerprint. Another
		// caller may already be using this one from a same-tag build, so never
		// delete it merely to recreate identical build input.
		return nil
	}
	return err
}

// readDepFiles reads dependency manifest files from the project directory.
// Returns a map of filename→content for files that exist.
func readDepFiles(projectDir string) map[string]string {
	files := make(map[string]string)
	for _, name := range buildDepFiles {
		data, err := os.ReadFile(filepath.Join(projectDir, name))
		if err != nil {
			continue
		}
		files[name] = string(data)
	}
	return files
}

// createDockerfileConfigMap creates a ConfigMap containing the Dockerfile.
func (k *K8sBackend) createDockerfileConfigMap(ctx context.Context, name string, dockerfile []byte) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "mcp-devbox",
				"devbox/build":                 "buildah",
			},
		},
		Data: map[string]string{
			"Dockerfile": string(dockerfile),
		},
	}
	_, err := k.clientset.CoreV1().ConfigMaps(k.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// ConfigMap names include the immutable image fingerprint. Another
		// caller may already be using this one from a same-tag build, so never
		// delete it merely to recreate identical build input.
		return nil
	}
	return err
}

// deleteConfigMap deletes a ConfigMap by name.
func (k *K8sBackend) deleteConfigMap(ctx context.Context, name string) error {
	err := k.clientset.CoreV1().ConfigMaps(k.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// CleanupBuilds deletes completed (Succeeded/Failed) build pods and their
// associated ConfigMaps older than maxAge. Returns the count of deleted pods.
func (k *K8sBackend) CleanupBuilds(ctx context.Context, maxAge time.Duration) (int, error) {
	pods, err := k.clientset.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "devbox/build=buildah",
	})
	if err != nil {
		return 0, fmt.Errorf("list build pods: %w", err)
	}

	cutoff := time.Now().Add(-maxAge)
	cleaned := 0

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		if pod.CreationTimestamp.After(cutoff) {
			continue
		}

		if err := k.deletePod(ctx, pod.Name); err != nil {
			continue
		}
		cleaned++
	}

	// Second pass: clean orphaned build ConfigMaps by label + age.
	// This catches ConfigMaps whose pods were already deleted or whose
	// names don't follow the expected naming convention.
	k.cleanupBuildConfigMaps(ctx, cutoff)

	return cleaned, nil
}

// cleanupBuildConfigMaps deletes build-labeled ConfigMaps older than cutoff.
func (k *K8sBackend) cleanupBuildConfigMaps(ctx context.Context, cutoff time.Time) {
	cms, err := k.clientset.CoreV1().ConfigMaps(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "devbox/build=buildah",
	})
	if err != nil {
		return
	}
	for i := range cms.Items {
		cm := &cms.Items[i]
		if cm.CreationTimestamp.After(cutoff) {
			continue
		}
		_ = k.deleteConfigMap(ctx, cm.Name)
	}
}

// sanitizeBuildName extracts a filesystem-safe name from an image tag.
func sanitizeBuildName(tag string) string {
	// Use the last path component, strip the registry prefix
	parts := strings.Split(tag, "/")
	name := parts[len(parts)-1]
	// Replace colons and other unsafe chars
	name = buildNameRe.ReplaceAllString(name, "-")
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func (k *K8sBackend) effectiveBuildTimeout() time.Duration {
	if k.buildTimeout <= 0 {
		return DefaultBuildTimeout
	}
	return k.buildTimeout
}
