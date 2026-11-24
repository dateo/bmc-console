package browser

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is just enough of the Kubernetes API for the launcher.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	secret   map[string]any
	pod      map[string]any
	patch    map[string]any
	gets     int
	deleted  []string
	failPod  bool
	podPhase string // phase reported once the Pod "starts"
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	decode := func() map[string]any {
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		return m
	}
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	base := "/api/v1/namespaces/browsers/"
	switch {
	case r.Method == "POST" && r.URL.Path == base+"secrets":
		f.secret = decode()
		reply(201, map[string]any{"metadata": map[string]any{"name": "bmc-browser-s1", "uid": "secret-uid"}})
	case r.Method == "POST" && r.URL.Path == base+"pods":
		if f.failPod {
			reply(403, map[string]any{"message": "pods is forbidden"})
			return
		}
		f.pod = decode()
		reply(201, map[string]any{"metadata": map[string]any{"name": "bmc-browser-p1", "uid": "pod-uid"}})
	case r.Method == "PATCH" && r.URL.Path == base+"secrets/bmc-browser-s1":
		if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
			reply(415, map[string]any{"message": ct})
			return
		}
		f.patch = decode()
		reply(200, map[string]any{})
	case r.Method == "GET" && r.URL.Path == base+"pods/bmc-browser-p1":
		f.gets++
		status := map[string]any{"phase": "Pending"}
		if f.gets >= 2 {
			status = map[string]any{"phase": f.podPhase, "podIP": "10.244.1.7", "message": "OOMKilled"}
		}
		reply(200, map[string]any{"metadata": map[string]any{"name": "bmc-browser-p1", "uid": "pod-uid"}, "status": status})
	case r.Method == "DELETE":
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, base))
		reply(200, map[string]any{})
	default:
		reply(404, map[string]any{"message": "not found"})
	}
}

func newKube(api *httptest.Server) *Kubernetes {
	return &Kubernetes{
		APIServer: api.URL, Token: "tok", Namespace: "browsers", Image: "registry.example/browser:1",
		RuntimeClassName: "gvisor", Labels: map[string]string{"role": "remote-browser"},
		OwnerPod: &OwnerRef{Name: "guacd-0", UID: "owner-uid"}, PollInterval: 10 * time.Millisecond,
	}
}

func lookup(m any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, _ := m.(map[string]any)
			m = mm[k]
		case int:
			s, _ := m.([]any)
			if k >= len(s) {
				return nil
			}
			m = s[k]
		}
	}
	return m
}

func TestKubernetesLaunch(t *testing.T) {
	f := &fakeAPI{podPhase: "Running"}
	api := httptest.NewServer(f)
	defer api.Close()
	k := newKube(api)
	inst, err := k.Launch(context.Background(), Spec{URL: "https://10.1.2.3/", Password: "pw123456", Session: "t_abc", MaxLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Addr() != "10.244.1.7:5900" {
		t.Fatalf("addr %q", inst.Addr())
	}

	// The password is only in the Secret.
	if got := lookup(f.secret, "stringData", "vnc-password"); got != "pw123456" {
		t.Fatalf("secret password %v", got)
	}
	raw, _ := json.Marshal(f.pod)
	if strings.Contains(string(raw), "pw123456") {
		t.Fatal("VNC password appears in the Pod spec")
	}
	spec := lookup(f.pod, "spec")
	c := lookup(spec, "containers", 0)
	var sawRef, sawURL bool
	for _, e := range lookup(c, "env").([]any) {
		switch lookup(e, "name") {
		case "VNC_PASSWORD":
			sawRef = lookup(e, "valueFrom", "secretKeyRef", "name") == "bmc-browser-s1"
		case "BROWSER_URL":
			sawURL = lookup(e, "value") == "https://10.1.2.3/"
		}
	}
	if !sawRef || !sawURL {
		t.Fatalf("env: secret ref %v, url %v", sawRef, sawURL)
	}
	for _, check := range []struct {
		path []any
		want any
	}{
		{[]any{"automountServiceAccountToken"}, false},
		{[]any{"enableServiceLinks"}, false},
		{[]any{"runtimeClassName"}, "gvisor"},
		{[]any{"activeDeadlineSeconds"}, float64(3600)},
		{[]any{"securityContext", "runAsNonRoot"}, true},
		{[]any{"securityContext", "seccompProfile", "type"}, "RuntimeDefault"},
		{[]any{"containers", 0, "securityContext", "readOnlyRootFilesystem"}, true},
		{[]any{"containers", 0, "securityContext", "allowPrivilegeEscalation"}, false},
		{[]any{"containers", 0, "securityContext", "capabilities", "drop", 0}, "ALL"},
	} {
		if got := lookup(spec, check.path...); got != check.want {
			t.Errorf("spec%v = %v, want %v", check.path, got, check.want)
		}
	}
	meta := lookup(f.pod, "metadata")
	if lookup(meta, "labels", "role") != "remote-browser" || lookup(meta, "labels", "bmc-console.io/session") != "t_abc" {
		t.Errorf("labels %v", lookup(meta, "labels"))
	}
	if lookup(meta, "ownerReferences", 0, "uid") != "owner-uid" {
		t.Errorf("pod owner %v", lookup(meta, "ownerReferences"))
	}
	// The Secret is handed to the Pod for garbage collection.
	if lookup(f.patch, "metadata", "ownerReferences", 0, "uid") != "pod-uid" {
		t.Errorf("secret owner %v", f.patch)
	}

	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	_ = inst.Close()
	if len(f.deleted) != 1 || f.deleted[0] != "pods/bmc-browser-p1" {
		t.Fatalf("deleted %v", f.deleted)
	}
}

func TestKubernetesLaunchCleansUp(t *testing.T) {
	t.Run("pod refused", func(t *testing.T) {
		f := &fakeAPI{failPod: true}
		api := httptest.NewServer(f)
		defer api.Close()
		_, err := newKube(api).Launch(context.Background(), Spec{URL: "https://10.1.2.3/", Password: "pw"})
		if err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("err %v", err)
		}
		if len(f.deleted) != 1 || f.deleted[0] != "secrets/bmc-browser-s1" {
			t.Fatalf("deleted %v", f.deleted)
		}
	})
	t.Run("pod failed", func(t *testing.T) {
		f := &fakeAPI{podPhase: "Failed"}
		api := httptest.NewServer(f)
		defer api.Close()
		_, err := newKube(api).Launch(context.Background(), Spec{URL: "https://10.1.2.3/", Password: "pw"})
		if err == nil || !strings.Contains(err.Error(), "OOMKilled") {
			t.Fatalf("err %v", err)
		}
		if len(f.deleted) != 1 || f.deleted[0] != "pods/bmc-browser-p1" {
			t.Fatalf("deleted %v", f.deleted)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := &fakeAPI{podPhase: "Pending"}
		api := httptest.NewServer(f)
		defer api.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, err := newKube(api).Launch(ctx, Spec{URL: "https://10.1.2.3/", Password: "pw"}); err == nil {
			t.Fatal("Launch succeeded for a Pod that never ran")
		}
		if len(f.deleted) != 1 {
			t.Fatalf("deleted %v", f.deleted)
		}
	})
	t.Run("bad url", func(t *testing.T) {
		f := &fakeAPI{}
		api := httptest.NewServer(f)
		defer api.Close()
		if _, err := newKube(api).Launch(context.Background(), Spec{URL: "file:///etc/passwd"}); err == nil {
			t.Fatal("file URL accepted")
		}
		if len(f.calls) != 0 {
			t.Fatalf("API called for an invalid URL: %v", f.calls)
		}
	})
}
