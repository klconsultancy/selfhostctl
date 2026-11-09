// Package flux gives Flux its git credential, checks that it syncs, and suspends and
// resumes it (docs/design.md#gitops). Flux itself comes from the cluster's own
// definition; nothing here installs it.
package flux

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/config"
)

var (
	KustomizationGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
	HelmReleaseGVK   = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
	FluxInstanceGVK  = schema.GroupVersionKind{Group: "fluxcd.controlplane.io", Version: "v1", Kind: "FluxInstance"}
	sourceGroup      = "source.toolkit.fluxcd.io"
)

const (
	// SuspendedAnnotation marks what Suspend suspended, so Resume resumes only that.
	SuspendedAnnotation = "selfhostctl.io/suspended"
	// RequestedAtAnnotation asks a Flux controller to reconcile now.
	RequestedAtAnnotation = "reconcile.fluxcd.io/requestedAt"
)

// ErrNoFlux: the Flux CRDs aren't on the cluster.
var ErrNoFlux = errors.New("no Flux CRDs on the Management cluster: is Flux in the cluster's definition?")

// Ref names one Flux object.
type Ref struct {
	Kind, Namespace, Name string
}

func (r Ref) String() string { return r.Kind + " " + r.Namespace + "/" + r.Name }

// --- the credential ---

// Credential is the Secret Flux's source reads over SSH.
type Credential struct {
	Namespace, Name      string
	Identity, KnownHosts string
}

// Secret is the credential as an object to apply. An empty field is an error: an empty
// value must never replace a working key.
func (cr Credential) Secret() (*unstructured.Unstructured, error) {
	switch {
	case cr.Identity == "":
		return nil, fmt.Errorf("Secret %s/%s: identity is empty; refusing to write it", cr.Namespace, cr.Name)
	case cr.KnownHosts == "":
		return nil, fmt.Errorf("Secret %s/%s: known_hosts is empty; refusing to write it", cr.Namespace, cr.Name)
	}
	s := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"namespace": cr.Namespace, "name": cr.Name},
		"type":       "Opaque",
		// data, not stringData: the API server folds stringData into data, so only data
		// compares with what is stored.
		"data": map[string]any{
			"identity":    base64.StdEncoding.EncodeToString([]byte(cr.Identity)),
			"known_hosts": base64.StdEncoding.EncodeToString([]byte(cr.KnownHosts)),
		},
	}}
	return s, nil
}

// --- sync ---

// Sync is the state of Flux's sync of the root Kustomization.
type Sync struct {
	// Waiting says what isn't synced yet; empty when synced.
	Waiting string
	// Revision is the root's source revision, when it has one.
	Revision string
}

// State judges the sync without changing anything: every FluxInstance Ready, the
// root's source Ready on its generation, and the root Ready on the source's revision.
// ErrNoFlux when the CRDs are missing.
func State(ctx context.Context, c ctrlclient.Client, root config.ObjectRef) (Sync, error) {
	instances := &unstructured.UnstructuredList{}
	instances.SetGroupVersionKind(FluxInstanceGVK)
	if err := c.List(ctx, instances); err != nil {
		if meta.IsNoMatchError(err) {
			return Sync{}, ErrNoFlux
		}
		return Sync{}, err
	}
	for i := range instances.Items {
		if ok, why := ready(&instances.Items[i]); !ok {
			return Sync{Waiting: why}, nil
		}
	}
	kustomization, src, err := rootAndSource(ctx, c, root)
	if err != nil || kustomization == nil || src == nil {
		w := ""
		if err == nil {
			w = fmt.Sprintf("no Kustomization %s/%s yet", root.Namespace, root.Name)
			if kustomization != nil {
				w = "no source for Kustomization " + root.Namespace + "/" + root.Name + " yet"
			}
		}
		return Sync{Waiting: w}, err
	}
	if ok, why := ready(src); !ok {
		return Sync{Waiting: why}, nil
	}
	rev, _, _ := unstructured.NestedString(src.Object, "status", "artifact", "revision")
	if ok, why := ready(kustomization); !ok {
		return Sync{Waiting: why, Revision: rev}, nil
	}
	if applied, _, _ := unstructured.NestedString(kustomization.Object, "status", "lastAppliedRevision"); applied != rev {
		return Sync{Waiting: fmt.Sprintf("%s applied %q, its source is at %q", describe(kustomization), applied, rev), Revision: rev}, nil
	}
	return Sync{Revision: rev}, nil
}

// rootAndSource fetches the root Kustomization and its source; nil for one that
// doesn't exist yet.
func rootAndSource(ctx context.Context, c ctrlclient.Client, root config.ObjectRef) (k, src *unstructured.Unstructured, err error) {
	k = &unstructured.Unstructured{}
	k.SetGroupVersionKind(KustomizationGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: root.Namespace, Name: root.Name}, k); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil, ErrNoFlux
		}
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	ref, _, _ := unstructured.NestedStringMap(k.Object, "spec", "sourceRef")
	if ref["kind"] == "" || ref["name"] == "" {
		return k, nil, fmt.Errorf("the root Kustomization %s/%s has no spec.sourceRef", root.Namespace, root.Name)
	}
	ns := ref["namespace"]
	if ns == "" {
		ns = root.Namespace
	}
	src = &unstructured.Unstructured{}
	src.SetGroupVersionKind(schema.GroupVersionKind{Group: sourceGroup, Version: "v1", Kind: ref["kind"]})
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref["name"]}, src); err != nil {
		if apierrors.IsNotFound(err) {
			return k, nil, nil
		}
		return k, nil, err
	}
	return k, src, nil
}

// ready reports whether a Flux object is Ready on its current generation, and why not.
func ready(u *unstructured.Unstructured) (bool, string) {
	gen, _, _ := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
	cond, ok := condition(u, "Ready")
	switch {
	case !ok:
		return false, describe(u) + " has no Ready condition yet"
	case gen != u.GetGeneration():
		return false, describe(u) + " hasn't seen its latest spec yet"
	case cond["status"] != "True":
		return false, fmt.Sprintf("%s isn't Ready: %v", describe(u), cond["message"])
	}
	return true, ""
}

func condition(u *unstructured.Unstructured, t string) (map[string]any, bool) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == t {
			return m, true
		}
	}
	return nil, false
}

func describe(u *unstructured.Unstructured) string {
	return u.GetKind() + " " + u.GetNamespace() + "/" + u.GetName()
}

// Reconcile makes Flux fetch the source and apply the root now, and waits for both: a
// Ready source on the branch's current revision, not the last good artifact. at is
// the request's value (a timestamp); poll is how often it looks. A source or root that
// fails the request fails at once with Flux's message.
func Reconcile(ctx context.Context, c ctrlclient.Client, root config.ObjectRef, at string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	wait := func(what func() (bool, error)) error {
		for {
			done, err := what()
			if err != nil || done {
				return err
			}
			if time.Now().After(deadline) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(poll):
			}
		}
	}

	// The CRDs and the root come with Flux's install, shortly after the cluster's.
	var k, src *unstructured.Unstructured
	var lastErr error
	if err := wait(func() (bool, error) {
		var err error
		k, src, err = rootAndSource(ctx, c, root)
		if errors.Is(err, ErrNoFlux) {
			lastErr = err
			return false, nil
		}
		return src != nil, err
	}); err != nil {
		return err
	}
	if src == nil {
		if lastErr != nil {
			return lastErr
		}
		return fmt.Errorf("no Kustomization %s/%s with a source after %s", root.Namespace, root.Name, timeout)
	}

	if err := request(ctx, c, src, at); err != nil {
		return err
	}
	if err := handled(ctx, c, src, at, wait); err != nil {
		return err
	}
	if err := request(ctx, c, k, at); err != nil {
		return err
	}
	if err := handled(ctx, c, k, at, wait); err != nil {
		return err
	}
	// Then FluxInstances and the revision check.
	var s Sync
	if err := wait(func() (bool, error) {
		var err error
		s, err = State(ctx, c, root)
		return err == nil && s.Waiting == "", err
	}); err != nil {
		return err
	}
	if s.Waiting != "" {
		return fmt.Errorf("not synced after %s: %s", timeout, s.Waiting)
	}
	return nil
}

// request asks Flux to reconcile obj now.
func request(ctx context.Context, c ctrlclient.Client, obj *unstructured.Unstructured, at string) error {
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, RequestedAtAnnotation, at)
	if err := c.Patch(ctx, obj, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("%s: requesting a reconcile: %w", describe(obj), err)
	}
	return nil
}

// handled waits until Flux handled the request at and reports the outcome: nil when
// Ready, an error with Flux's message when not.
func handled(ctx context.Context, c ctrlclient.Client, obj *unstructured.Unstructured, at string, wait func(func() (bool, error)) error) error {
	key := ctrlclient.ObjectKeyFromObject(obj)
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	seen := false
	var why string
	if err := wait(func() (bool, error) {
		if err := c.Get(ctx, key, live); err != nil {
			return false, err
		}
		h, _, _ := unstructured.NestedString(live.Object, "status", "lastHandledReconcileAt")
		if h != at {
			return false, nil
		}
		seen = true
		var ok bool
		ok, why = ready(live)
		if ok {
			return true, nil
		}
		if r, _ := condition(live, "Reconciling"); r["status"] == "True" {
			return false, nil
		}
		return false, errors.New(why)
	}); err != nil {
		return err
	}
	switch {
	case !seen:
		return fmt.Errorf("%s didn't handle the reconcile request in time: is its controller running?", describe(obj))
	case why != "":
		return errors.New(why)
	}
	return nil
}

// --- suspend and resume ---

// Suspend sets spec.suspend on every Kustomization and HelmRelease in the cluster: the
// root first, so its self-sync can't undo the rest, then the other Kustomizations,
// then the HelmReleases. It marks what it suspends; one already suspended is left
// alone. It returns what it suspended, in order.
func Suspend(ctx context.Context, c ctrlclient.Client, root config.ObjectRef) ([]Ref, error) {
	objs, err := ordered(ctx, c, root)
	if err != nil {
		return nil, err
	}
	var done []Ref
	for _, o := range objs {
		if suspended(o) {
			continue
		}
		patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:"true"}},"spec":{"suspend":true}}`, SuspendedAnnotation)
		if err := c.Patch(ctx, o, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
			return done, fmt.Errorf("suspending %s: %w", describe(o), err)
		}
		done = append(done, refOf(o))
	}
	return done, nil
}

// Resume clears spec.suspend on what Suspend marked, in reverse: the root last. One the
// operator suspended stays suspended. It returns what it resumed, in order.
func Resume(ctx context.Context, c ctrlclient.Client, root config.ObjectRef) ([]Ref, error) {
	objs, err := ordered(ctx, c, root)
	if err != nil {
		return nil, err
	}
	var done []Ref
	for i := len(objs) - 1; i >= 0; i-- {
		o := objs[i]
		if o.GetAnnotations()[SuspendedAnnotation] == "" {
			continue
		}
		patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:null}},"spec":{"suspend":false}}`, SuspendedAnnotation)
		if err := c.Patch(ctx, o, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
			return done, fmt.Errorf("resuming %s: %w", describe(o), err)
		}
		done = append(done, refOf(o))
	}
	return done, nil
}

// Suspended lists every suspended Kustomization and HelmRelease. None without the
// CRDs.
func Suspended(ctx context.Context, c ctrlclient.Client) ([]Ref, error) {
	objs, err := ordered(ctx, c, config.ObjectRef{})
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, o := range objs {
		if suspended(o) {
			refs = append(refs, refOf(o))
		}
	}
	return refs, nil
}

// ordered lists the root, the other Kustomizations, then the HelmReleases, each by
// namespace and name. A missing CRD contributes nothing.
func ordered(ctx context.Context, c ctrlclient.Client, root config.ObjectRef) ([]*unstructured.Unstructured, error) {
	var first, rest []*unstructured.Unstructured
	for _, gvk := range []schema.GroupVersionKind{KustomizationGVK, HelmReleaseGVK} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)
		if err := c.List(ctx, list); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return nil, err
		}
		items := list.Items
		sortItems(items)
		for i := range items {
			o := &items[i]
			if gvk == KustomizationGVK && o.GetNamespace() == root.Namespace && o.GetName() == root.Name {
				first = append(first, o)
				continue
			}
			rest = append(rest, o)
		}
	}
	return append(first, rest...), nil
}

func suspended(u *unstructured.Unstructured) bool {
	s, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend")
	return s
}

func refOf(u *unstructured.Unstructured) Ref {
	return Ref{Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName()}
}

func sortItems(items []unstructured.Unstructured) {
	slices.SortFunc(items, func(a, b unstructured.Unstructured) int {
		return cmp.Or(cmp.Compare(a.GetNamespace(), b.GetNamespace()), cmp.Compare(a.GetName(), b.GetName()))
	})
}
