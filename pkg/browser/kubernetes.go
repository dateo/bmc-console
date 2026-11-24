package browser

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Kubernetes launches each browser as a Pod in one namespace, through the
// API server's REST interface (no client library). The broker's service
// account needs create/get/delete on pods and create/patch/delete on secrets
// in that namespace; see examples/kubernetes/browser-rbac.yaml.
//
// The VNC password goes into a per-session Secret that the Pod reads, so it
// is not visible in the Pod spec. The Secret is owned by the Pod and the Pod
// by the broker's own Pod (when OwnerPod is set), so Kubernetes garbage
// collects both if the broker disappears; activeDeadlineSeconds bounds a Pod
// whose broker hangs.
type Kubernetes struct {
	// APIServer is the API base URL, e.g. https://10.0.0.1:443.
	APIServer string
	// Token is the bearer token. TokenFile, when set, is re-read on every
	// request so projected (rotating) tokens keep working.
	Token     string
	TokenFile string
	// HTTPClient talks to the API server. InCluster sets one that trusts
	// the cluster CA.
	HTTPClient *http.Client

	// Namespace receives the browser Pods.
	Namespace string
	// Image is the browser image.
	Image string
	// ImagePullPolicy defaults to IfNotPresent.
	ImagePullPolicy string
	// RuntimeClassName selects a sandboxed runtime such as gVisor or Kata.
	RuntimeClassName string
	// SeccompLocalhostProfile selects a node-local seccomp profile instead
	// of RuntimeDefault, e.g. one that allows Chromium's user-namespace
	// sandbox.
	SeccompLocalhostProfile string
	// AppArmorLocalhostProfile selects an AppArmor profile loaded on the
	// nodes (Kubernetes 1.30+). Needed where the host restricts
	// unprivileged user namespaces (Ubuntu's
	// kernel.apparmor_restrict_unprivileged_userns=1): the profile must
	// allow `userns,` for Chromium's sandbox.
	AppArmorLocalhostProfile string
	// CPURequest, CPULimit, MemoryRequest and MemoryLimit are Kubernetes
	// quantities. Defaults: 250m, 1, 512Mi, 1Gi.
	CPURequest, CPULimit, MemoryRequest, MemoryLimit string
	// Env adds container environment variables, e.g. BROWSER_NO_SANDBOX=1
	// under a runtime class that is itself the sandbox.
	Env map[string]string
	// Labels are added to every browser Pod, e.g. for NetworkPolicy
	// selectors. The Pods always carry app.kubernetes.io/name=bmc-browser.
	Labels map[string]string
	// OwnerPod makes the broker's Pod the owner of every browser Pod.
	OwnerPod *OwnerRef
	// PollInterval is how often Launch checks whether the Pod has an IP.
	// Default 500ms.
	PollInterval time.Duration
}

// OwnerRef names the broker's own Pod (from the downward API).
type OwnerRef struct {
	Name string
	UID  string
}

const (
	saDir        = "/var/run/secrets/kubernetes.io/serviceaccount"
	podNameLabel = "app.kubernetes.io/name"
	podName      = "bmc-browser"
	managedBy    = "app.kubernetes.io/managed-by"
	sessionLabel = "bmc-console.io/session"
)

// InCluster fills in the API server, token and CA from the service account
// mounted into the broker's Pod, and the namespace unless already set.
func (k *Kubernetes) InCluster() error {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return errors.New("browser: not running in a Kubernetes Pod (KUBERNETES_SERVICE_HOST unset)")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return fmt.Errorf("browser: read cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return errors.New("browser: cluster CA holds no certificate")
	}
	k.APIServer = "https://" + net.JoinHostPort(host, port)
	k.TokenFile = saDir + "/token"
	k.HTTPClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	if k.Namespace == "" {
		ns, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			return fmt.Errorf("browser: read namespace: %w", err)
		}
		k.Namespace = strings.TrimSpace(string(ns))
	}
	return nil
}

// Launch implements Launcher.
func (k *Kubernetes) Launch(ctx context.Context, s Spec) (Instance, error) {
	if _, err := ValidateURL(s.URL); err != nil {
		return nil, err
	}
	if k.Image == "" || k.Namespace == "" {
		return nil, errors.New("browser: Kubernetes launcher needs Image and Namespace")
	}
	s = s.withDefaults()

	var secret object
	if err := k.do(ctx, http.MethodPost, k.path("secrets", ""), k.secretManifest(s), &secret); err != nil {
		return nil, fmt.Errorf("browser: create secret: %w", err)
	}
	var pod object
	if err := k.do(ctx, http.MethodPost, k.path("pods", ""), k.podManifest(s, secret.Metadata.Name), &pod); err != nil {
		k.deleteQuietly("secrets", secret.Metadata.Name)
		return nil, fmt.Errorf("browser: create pod: %w", err)
	}
	inst := &kubeInstance{k: k, pod: pod.Metadata.Name}
	// The Secret follows the Pod: Kubernetes deletes it with the Pod.
	owner := map[string]any{"metadata": map[string]any{"ownerReferences": []map[string]any{{
		"apiVersion": "v1", "kind": "Pod", "name": pod.Metadata.Name, "uid": pod.Metadata.UID,
		"blockOwnerDeletion": false,
	}}}}
	if err := k.patch(ctx, k.path("secrets", secret.Metadata.Name), owner); err != nil {
		k.deleteQuietly("secrets", secret.Metadata.Name)
		inst.Close()
		return nil, fmt.Errorf("browser: adopt secret: %w", err)
	}

	interval := k.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	for {
		if err := k.do(ctx, http.MethodGet, k.path("pods", pod.Metadata.Name), nil, &pod); err != nil {
			inst.Close()
			return nil, fmt.Errorf("browser: read pod: %w", err)
		}
		switch pod.Status.Phase {
		case "Failed", "Succeeded":
			inst.Close()
			return nil, fmt.Errorf("browser: pod %s ended (%s) before it served: %s", pod.Metadata.Name, pod.Status.Phase, pod.Status.Message)
		}
		if pod.Status.PodIP != "" && pod.Status.Phase == "Running" {
			inst.addr = net.JoinHostPort(pod.Status.PodIP, fmt.Sprint(VNCPort))
			return inst, nil
		}
		select {
		case <-ctx.Done():
			inst.Close()
			return nil, fmt.Errorf("browser: pod %s not running: %w", pod.Metadata.Name, ctx.Err())
		case <-time.After(interval):
		}
	}
}

func (k *Kubernetes) labels(s Spec) map[string]string {
	l := map[string]string{podNameLabel: podName, managedBy: "bmc-broker"}
	if s.Session != "" {
		l[sessionLabel] = s.Session
	}
	for key, v := range k.Labels {
		l[key] = v
	}
	return l
}

func (k *Kubernetes) secretManifest(s Spec) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"generateName": "bmc-browser-", "labels": k.labels(s)},
		"type":       "Opaque",
		"stringData": map[string]string{"vnc-password": s.Password},
	}
}

func (k *Kubernetes) podManifest(s Spec, secretName string) map[string]any {
	env := []map[string]any{{
		"name":      "VNC_PASSWORD",
		"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secretName, "key": "vnc-password"}},
	}}
	for name, v := range s.env() {
		if name != "VNC_PASSWORD" {
			env = append(env, map[string]any{"name": name, "value": v})
		}
	}
	for name, v := range k.Env {
		env = append(env, map[string]any{"name": name, "value": v})
	}
	seccomp := map[string]any{"type": "RuntimeDefault"}
	if k.SeccompLocalhostProfile != "" {
		seccomp = map[string]any{"type": "Localhost", "localhostProfile": k.SeccompLocalhostProfile}
	}
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	podSecurity := map[string]any{
		"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000,
		"seccompProfile": seccomp,
	}
	if k.AppArmorLocalhostProfile != "" {
		podSecurity["appArmorProfile"] = map[string]any{"type": "Localhost", "localhostProfile": k.AppArmorLocalhostProfile}
	}
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"activeDeadlineSeconds":         int64(s.MaxLifetime / time.Second),
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"terminationGracePeriodSeconds": 2,
		"securityContext":               podSecurity,
		"containers": []map[string]any{{
			"name":            "browser",
			"image":           k.Image,
			"imagePullPolicy": or(k.ImagePullPolicy, "IfNotPresent"),
			"env":             env,
			"ports":           []map[string]any{{"name": "vnc", "containerPort": VNCPort}},
			"readinessProbe": map[string]any{
				"tcpSocket": map[string]any{"port": VNCPort}, "periodSeconds": 1,
			},
			"resources": map[string]any{
				"requests": map[string]string{"cpu": or(k.CPURequest, "250m"), "memory": or(k.MemoryRequest, "512Mi")},
				"limits":   map[string]string{"cpu": or(k.CPULimit, "1"), "memory": or(k.MemoryLimit, "1Gi")},
			},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"readOnlyRootFilesystem":   true,
				"capabilities":             map[string]any{"drop": []string{"ALL"}},
			},
			"volumeMounts": []map[string]any{
				{"name": "tmp", "mountPath": "/tmp"},
				{"name": "shm", "mountPath": "/dev/shm"},
			},
		}},
		"volumes": []map[string]any{
			{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "512Mi"}},
			{"name": "shm", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "256Mi"}},
		},
	}
	if k.RuntimeClassName != "" {
		spec["runtimeClassName"] = k.RuntimeClassName
	}
	meta := map[string]any{"generateName": "bmc-browser-", "labels": k.labels(s)}
	if k.OwnerPod != nil && k.OwnerPod.Name != "" && k.OwnerPod.UID != "" {
		meta["ownerReferences"] = []map[string]any{{
			"apiVersion": "v1", "kind": "Pod", "name": k.OwnerPod.Name, "uid": k.OwnerPod.UID,
			"blockOwnerDeletion": false,
		}}
	}
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": spec}
}

type kubeInstance struct {
	k    *Kubernetes
	pod  string
	addr string
	once sync.Once
	err  error
}

func (i *kubeInstance) Addr() string { return i.addr }

func (i *kubeInstance) Close() error {
	i.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		i.err = i.k.delete(ctx, "pods", i.pod)
	})
	return i.err
}

// object is the subset of a Pod or Secret the launcher reads.
type object struct {
	Metadata struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"metadata"`
	Status struct {
		Phase   string `json:"phase"`
		PodIP   string `json:"podIP"`
		Message string `json:"message"`
	} `json:"status"`
}

func (k *Kubernetes) path(resource, name string) string {
	p := "/api/v1/namespaces/" + url.PathEscape(k.Namespace) + "/" + resource
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func (k *Kubernetes) delete(ctx context.Context, resource, name string) error {
	body := map[string]any{"gracePeriodSeconds": 0, "propagationPolicy": "Background"}
	err := k.do(ctx, http.MethodDelete, k.path(resource, name), body, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil
	}
	return err
}

func (k *Kubernetes) deleteQuietly(resource, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = k.delete(ctx, resource, name)
}

func (k *Kubernetes) patch(ctx context.Context, path string, body any) error {
	return k.request(ctx, http.MethodPatch, path, "application/merge-patch+json", body, nil)
}

func (k *Kubernetes) do(ctx context.Context, method, path string, body, out any) error {
	return k.request(ctx, method, path, "application/json", body, out)
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.msg) }

func (k *Kubernetes) request(ctx context.Context, method, path, contentType string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(k.APIServer, "/")+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	token := k.Token
	if k.TokenFile != "" {
		b, err := os.ReadFile(k.TokenFile)
		if err != nil {
			return fmt.Errorf("read token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := k.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var st struct {
			Message string `json:"message"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &st) == nil && st.Message != "" {
			msg = st.Message
		}
		return &statusError{code: resp.StatusCode, msg: msg}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
