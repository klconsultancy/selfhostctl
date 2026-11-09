package flux

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/klconsultancy/selfhostctl/internal/config"
)

var (
	gitRepositoryGVK = schema.GroupVersionKind{Group: sourceGroup, Version: "v1", Kind: "GitRepository"}
	root             = config.ObjectRef{Namespace: "flux-system", Name: "flux-system"}
)

func newFake(withCRDs bool, funcs interceptor.Funcs, objs ...ctrlclient.Object) ctrlclient.WithWatch {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	gvks := []schema.GroupVersionKind{KustomizationGVK, HelmReleaseGVK, FluxInstanceGVK, gitRepositoryGVK}
	var gvs []schema.GroupVersion
	for _, g := range gvks {
		gvs = append(gvs, g.GroupVersion())
	}
	mapper := meta.NewDefaultRESTMapper(gvs)
	for _, g := range gvks {
		mapper.Add(g, meta.RESTScopeNamespace)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...).Build()
	if !withCRDs {
		// The fake client doesn't consult its mapper on reads; a real API server answers
		// a missing CRD with no match.
		noMatch := func(o runtime.Object) error {
			gvk := o.GetObjectKind().GroupVersionKind()
			return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
		}
		funcs.Get = func(_ context.Context, _ ctrlclient.WithWatch, _ ctrlclient.ObjectKey, o ctrlclient.Object, _ ...ctrlclient.GetOption) error {
			return noMatch(o)
		}
		funcs.List = func(_ context.Context, _ ctrlclient.WithWatch, l ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
			return noMatch(l)
		}
	}
	return interceptor.NewClient(c, funcs)
}

func obj(gvk schema.GroupVersionKind, ns, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func readyStatus(extra map[string]any) map[string]any {
	s := map[string]any{
		"observedGeneration": int64(0),
		"conditions":         []any{map[string]any{"type": "Ready", "status": "True", "message": "ok"}},
	}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

// FluxInstance reports its generation on the Ready condition, not in status.
func TestReadyReadsTheConditionsGeneration(t *testing.T) {
	inst := func(condGen int64) *unstructured.Unstructured {
		u := obj(FluxInstanceGVK, "flux-system", "flux", map[string]any{"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": condGen}},
		}})
		u.SetGeneration(3)
		return u
	}
	if ok, why := ready(inst(3)); !ok {
		t.Errorf("Ready on generation 3: %s", why)
	}
	if ok, _ := ready(inst(2)); ok {
		t.Error("Ready on an older generation counted")
	}
}

func TestCredentialRefusesEmptyFields(t *testing.T) {
	for _, cr := range []Credential{
		{Namespace: "flux-system", Name: "git", KnownHosts: "host ssh-ed25519 AAAA"},
		{Namespace: "flux-system", Name: "git", Identity: "key"},
	} {
		if _, err := cr.Secret(); err == nil {
			t.Errorf("%+v: an empty field must be refused", cr)
		}
	}
	s, err := Credential{Namespace: "flux-system", Name: "git", Identity: "key", KnownHosts: "kh"}.Secret()
	if err != nil {
		t.Fatal(err)
	}
	if d, _, _ := unstructured.NestedStringMap(s.Object, "data"); d["identity"] != "a2V5" || d["known_hosts"] != "a2g=" {
		t.Errorf("stringData %v", d)
	}
}

func TestSuspendRootFirstResumeRootLast(t *testing.T) {
	var mu sync.Mutex
	var patched []string
	c := newFake(true, interceptor.Funcs{Patch: func(ctx context.Context, c ctrlclient.WithWatch, o ctrlclient.Object, p ctrlclient.Patch, opts ...ctrlclient.PatchOption) error {
		mu.Lock()
		patched = append(patched, o.GetNamespace()+"/"+o.GetName())
		mu.Unlock()
		return c.Patch(ctx, o, p, opts...)
	}},
		obj(KustomizationGVK, "apps", "a", nil),
		obj(KustomizationGVK, "flux-system", "flux-system", nil),
		obj(KustomizationGVK, "apps", "b", map[string]any{"spec": map[string]any{"suspend": true}}), // the operator's
		obj(HelmReleaseGVK, "apps", "chart", nil),
		obj(HelmReleaseGVK, "infra", "chart", nil),
	)
	ctx := context.Background()
	got, err := Suspend(ctx, c, root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"flux-system/flux-system", "apps/a", "apps/chart", "infra/chart"}
	if !reflect.DeepEqual(patched, want) {
		t.Errorf("suspend order %v, want %v", patched, want)
	}
	if len(got) != 4 || got[0].String() != "Kustomization flux-system/flux-system" {
		t.Errorf("suspended %v", got)
	}
	all, err := Suspended(ctx, c)
	if err != nil || len(all) != 5 {
		t.Errorf("Suspended: %v %v, want all five", all, err)
	}
	// Twice is a no-op.
	if again, err := Suspend(ctx, c, root); err != nil || len(again) != 0 {
		t.Errorf("second Suspend: %v %v", again, err)
	}

	patched = nil
	if _, err := Resume(ctx, c, root); err != nil {
		t.Fatal(err)
	}
	if want := []string{"infra/chart", "apps/chart", "apps/a", "flux-system/flux-system"}; !reflect.DeepEqual(patched, want) {
		t.Errorf("resume order %v, want %v", patched, want)
	}
	left, _ := Suspended(ctx, c)
	if len(left) != 1 || left[0].Name != "b" {
		t.Errorf("after Resume, suspended %v: only the operator's own suspend stays", left)
	}
	b := obj(KustomizationGVK, "apps", "b", nil)
	_ = c.Get(ctx, ctrlclient.ObjectKeyFromObject(b), b)
	if _, ok := b.GetAnnotations()[SuspendedAnnotation]; ok {
		t.Error("the operator's suspend was marked")
	}
}

func TestSuspendWithoutCRDs(t *testing.T) {
	c := newFake(false, interceptor.Funcs{})
	if got, err := Suspend(context.Background(), c, root); err != nil || len(got) != 0 {
		t.Errorf("Suspend: %v %v", got, err)
	}
	if got, err := Suspended(context.Background(), c); err != nil || len(got) != 0 {
		t.Errorf("Suspended: %v %v", got, err)
	}
}

// synced is a Flux that synced revision main@sha1:abc.
func synced(rootApplied string) []ctrlclient.Object {
	return []ctrlclient.Object{
		obj(FluxInstanceGVK, "flux-system", "flux", map[string]any{"status": readyStatus(nil)}),
		obj(gitRepositoryGVK, "flux-system", "flux-system", map[string]any{"status": readyStatus(map[string]any{
			"artifact": map[string]any{"revision": "main@sha1:abc"},
		})}),
		obj(KustomizationGVK, "flux-system", "flux-system", map[string]any{
			"spec":   map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"}},
			"status": readyStatus(map[string]any{"lastAppliedRevision": rootApplied}),
		}),
	}
}

func TestState(t *testing.T) {
	ctx := context.Background()
	if _, err := State(ctx, newFake(false, interceptor.Funcs{}), root); !errors.Is(err, ErrNoFlux) {
		t.Errorf("without CRDs: %v, want ErrNoFlux", err)
	}
	s, err := State(ctx, newFake(true, interceptor.Funcs{}, synced("main@sha1:abc")...), root)
	if err != nil || s.Waiting != "" || s.Revision != "main@sha1:abc" {
		t.Errorf("synced: %+v %v", s, err)
	}
	s, _ = State(ctx, newFake(true, interceptor.Funcs{}, synced("main@sha1:old")...), root)
	if !strings.Contains(s.Waiting, `applied "main@sha1:old"`) {
		t.Errorf("a root on an older revision: %+v", s)
	}
	s, _ = State(ctx, newFake(true, interceptor.Funcs{}, synced("main@sha1:abc")[0]), root)
	if !strings.Contains(s.Waiting, "no Kustomization flux-system/flux-system") {
		t.Errorf("no root: %+v", s)
	}
}

// controller plays Flux: it handles reconcile requests, with outcome as the Ready
// condition it reports.
func controller(ctx context.Context, c ctrlclient.Client, outcome func(kind string) map[string]any) {
	for ctx.Err() == nil {
		for _, gvk := range []schema.GroupVersionKind{gitRepositoryGVK, KustomizationGVK} {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(gvk)
			_ = c.List(ctx, list)
			for i := range list.Items {
				o := &list.Items[i]
				at := o.GetAnnotations()[RequestedAtAnnotation]
				// A handled request is settled once it has a Ready condition.
				_, settled := condition(o, "Ready")
				if h, _, _ := unstructured.NestedString(o.Object, "status", "lastHandledReconcileAt"); at == "" || (h == at && settled) {
					continue
				}
				_ = unstructured.SetNestedField(o.Object, at, "status", "lastHandledReconcileAt")
				_ = unstructured.SetNestedSlice(o.Object, []any{outcome(gvk.Kind)}, "status", "conditions")
				_ = c.Update(ctx, o)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReconcile(t *testing.T) {
	ok := func(string) map[string]any { return map[string]any{"type": "Ready", "status": "True"} }
	tests := []struct {
		name    string
		outcome func(string) map[string]any
		objs    []ctrlclient.Object
		crds    bool
		wantErr string
	}{
		{"a fresh fetch and apply", ok, synced("main@sha1:abc"), true, ""},
		{"the fetch fails: Flux's message", func(kind string) map[string]any {
			if kind == "GitRepository" {
				return map[string]any{"type": "Ready", "status": "False", "reason": "AuthenticationFailed", "message": "ssh: handshake failed"}
			}
			return ok(kind)
		}, synced("main@sha1:abc"), true, "ssh: handshake failed"},
		{"handled, but no Ready condition yet: wait, don't fail", func() func(string) map[string]any {
			var mu sync.Mutex
			n := map[string]int{}
			return func(kind string) map[string]any {
				mu.Lock()
				defer mu.Unlock()
				if n[kind]++; n[kind] == 1 {
					return map[string]any{"type": "Reconciling", "status": "False"}
				}
				return ok(kind)
			}
		}(), synced("main@sha1:abc"), true, ""},
		{"no Flux", ok, nil, false, "is Flux in the cluster's definition?"},
		{"no controller handles the request", nil, synced("main@sha1:abc"), true, "didn't handle the reconcile request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := newFake(tt.crds, interceptor.Funcs{}, tt.objs...)
			if tt.outcome != nil {
				go controller(ctx, c, tt.outcome)
			}
			err := Reconcile(ctx, c, root, "2026-11-09T10:00:00Z", 300*time.Millisecond, 5*time.Millisecond)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Reconcile: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Reconcile: %v, want %q", err, tt.wantErr)
			}
		})
	}
}
