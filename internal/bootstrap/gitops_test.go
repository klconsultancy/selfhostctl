package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/klconsultancy/selfhostctl/internal/flux"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

var gitRepositoryGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}

const fluxKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n-----END OPENSSH PRIVATE KEY-----\n"

func gitopsYAML(t *testing.T) string {
	t.Helper()
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte("git.example ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return "gitops:\n  flux:\n    credentialSecret: {name: flux-git, namespace: flux-system, fromEnv: FLUX_KEY, knownHosts: " + kh + "}\n"
}

func fluxObj(gvk schema.GroupVersionKind, name string, content map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace("flux-system")
	u.SetName(name)
	return u
}

// fluxObjects is Flux as the cluster's definition installs it, before it has a
// credential: its source can't fetch.
func fluxObjects() []ctrlclient.Object {
	ready := []any{map[string]any{"type": "Ready", "status": "True"}}
	return []ctrlclient.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "flux-system"}},
		fluxObj(flux.FluxInstanceGVK, "flux", map[string]any{"status": map[string]any{"conditions": ready}}),
		fluxObj(gitRepositoryGVK, "flux-system", map[string]any{"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "False", "message": "secret 'flux-git' not found"}},
		}}),
		fluxObj(flux.KustomizationGVK, "flux-system", map[string]any{
			"spec": map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"}},
		}),
	}
}

// fluxController plays Flux: once the credential exists, a reconcile request fetches
// revision main@sha1:abc and the root applies it.
func fluxController(ctx context.Context, c ctrlclient.Client) {
	for ctx.Err() == nil {
		hasKey := c.Get(ctx, ctrlclient.ObjectKey{Namespace: "flux-system", Name: "flux-git"}, &corev1.Secret{}) == nil
		for _, gvk := range []schema.GroupVersionKind{gitRepositoryGVK, flux.KustomizationGVK} {
			o := fluxObj(gvk, "flux-system", map[string]any{})
			if c.Get(ctx, ctrlclient.ObjectKeyFromObject(o), o) != nil {
				continue
			}
			at := o.GetAnnotations()[flux.RequestedAtAnnotation]
			if h, _, _ := unstructured.NestedString(o.Object, "status", "lastHandledReconcileAt"); at == "" || h == at || !hasKey {
				continue
			}
			_ = unstructured.SetNestedField(o.Object, at, "status", "lastHandledReconcileAt")
			_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
			if gvk == gitRepositoryGVK {
				_ = unstructured.SetNestedField(o.Object, "main@sha1:abc", "status", "artifact", "revision")
			} else {
				_ = unstructured.SetNestedField(o.Object, "main@sha1:abc", "status", "lastAppliedRevision")
			}
			_ = c.Update(ctx, o)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func only(ps []phase.Phase, name string) []phase.Phase {
	for _, p := range ps {
		if p.Name() == name {
			return []phase.Phase{p}
		}
	}
	return nil
}

func TestGitOpsWithoutConfig(t *testing.T) {
	w := &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: newFake(theCluster(false), kubeconfigSecret(managementKC_))}
	if st := statusOf(t, w.phases(t, false), "gitops"); st.State != phase.Done || st.Reason != "no gitops in the config" {
		t.Errorf("gitops: %+v", st)
	}
}

func TestGitOpsBeforeThePivot(t *testing.T) {
	w := &world{docker: running(t), bootstrap: newFake(theCluster(false)), kubeconfig: kubeconfigFile(t), management: newFake(),
		extraYAML: gitopsYAML(t), env: map[string]string{"FLUX_KEY": fluxKey}}
	if st := statusOf(t, w.phases(t, false), "gitops"); st.State != phase.Blocked || !strings.Contains(st.Reason, "doesn't manage itself yet") {
		t.Errorf("gitops before the pivot: %+v, want blocked", st)
	}
}

func TestGitOpsSync(t *testing.T) {
	mgmt := newFake(append([]ctrlclient.Object{theCluster(false), kubeconfigSecret(managementKC_)}, fluxObjects()...)...)
	w := &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: mgmt,
		extraYAML: gitopsYAML(t), env: map[string]string{"FLUX_KEY": fluxKey}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go fluxController(ctx, mgmt)

	if st := statusOf(t, w.phases(t, false), "gitops"); st.State != phase.Next {
		t.Fatalf("without the credential: %+v, want next", st)
	}
	var out bytes.Buffer
	if err := phase.Run(ctx, only(w.phases(t, false), "gitops"), "", &out); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "gitops: done (synced main@sha1:abc)") {
		t.Errorf("output %q", out.String())
	}
	if strings.Contains(out.String(), "secret") {
		t.Errorf("the key reached the output: %q", out.String())
	}
	s := &corev1.Secret{}
	if err := mgmt.Get(ctx, ctrlclient.ObjectKey{Namespace: "flux-system", Name: "flux-git"}, s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["identity"]) != fluxKey || !strings.HasPrefix(string(s.Data["known_hosts"]), "git.example ") {
		t.Errorf("Secret data %v", s.Data)
	}

	// Again: nothing runs, nothing is written.
	out.Reset()
	if err := phase.Run(ctx, only(w.phases(t, false), "gitops"), "", &out); err != nil || strings.Contains(out.String(), "running") {
		t.Errorf("second run: %v %q, want done without running", err, out.String())
	}
	again := &corev1.Secret{}
	_ = mgmt.Get(ctx, ctrlclient.ObjectKeyFromObject(s), again)
	if again.ResourceVersion != s.ResourceVersion {
		t.Error("the second run rewrote the Secret")
	}
}

func TestGitOpsRefusesAnEmptyKey(t *testing.T) {
	mgmt := newFake(append([]ctrlclient.Object{theCluster(false), kubeconfigSecret(managementKC_)}, fluxObjects()...)...)
	w := &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: mgmt,
		extraYAML: gitopsYAML(t), env: map[string]string{"FLUX_KEY": ""}}
	err := phase.Run(t.Context(), only(w.phases(t, false), "gitops"), "", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "identity is empty") {
		t.Errorf("Run: %v, want the empty key refused", err)
	}
	if mgmt.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "flux-system", Name: "flux-git"}, &corev1.Secret{}) == nil {
		t.Error("a Secret was written")
	}
}

func TestGitOpsWithoutFlux(t *testing.T) {
	noFlux := func(o runtime.Object) error {
		gvk := o.GetObjectKind().GroupVersionKind()
		if strings.Contains(gvk.Group, "fluxcd") {
			return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
		}
		return nil
	}
	base := newFake(theCluster(false), kubeconfigSecret(managementKC_)).(ctrlclient.WithWatch)
	mgmt := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c ctrlclient.WithWatch, k ctrlclient.ObjectKey, o ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			if err := noFlux(o); err != nil {
				return err
			}
			return c.Get(ctx, k, o, opts...)
		},
		List: func(ctx context.Context, c ctrlclient.WithWatch, l ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			if err := noFlux(l); err != nil {
				return err
			}
			return c.List(ctx, l, opts...)
		},
	})
	w := &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: mgmt,
		extraYAML: gitopsYAML(t), env: map[string]string{"FLUX_KEY": fluxKey}}
	err := phase.Run(t.Context(), only(w.phases(t, false), "gitops"), "", &bytes.Buffer{})
	var b *phase.BlockedError
	if !errors.As(err, &b) || !strings.Contains(b.Reason, "is Flux in the cluster's definition?") {
		t.Errorf("Run: %v, want blocked", err)
	}
}
