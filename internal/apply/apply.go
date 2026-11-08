// Package apply applies Kubernetes objects server-side and tells whether they are in
// place, for both the beforeApply entries and the Management cluster's own manifests.
package apply

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// FieldManager owns what the tool applies server-side.
const FieldManager = "selfhostctl"

// Redactor removes secret values from an error message.
type Redactor func(string) string

// None redacts nothing.
func None(s string) string { return s }

// Decode reads every object in a YAML or JSON stream, skipping empty documents.
func Decode(r io.Reader, redact Redactor) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	var objs []*unstructured.Unstructured
	for {
		var raw map[string]any
		if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, errors.New(redact(err.Error()))
		}
		if len(raw) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: raw}
		if u.GetAPIVersion() == "" || u.GetKind() == "" || u.GetName() == "" {
			return nil, errors.New("every object needs apiVersion, kind and metadata.name")
		}
		objs = append(objs, u)
	}
	return objs, nil
}

// Load reads the objects at path: a kustomization directory (built in-process), a
// directory of YAML files (sorted by name), or one file.
func Load(path string) ([]*unstructured.Unstructured, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return Decode(f, None)
	}
	for _, k := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if _, err := os.Stat(filepath.Join(path, k)); err == nil {
			res, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), path)
			if err != nil {
				return nil, fmt.Errorf("kustomize %s: %w", path, err)
			}
			data, err := res.AsYaml()
			if err != nil {
				return nil, err
			}
			return Decode(strings.NewReader(string(data)), None)
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	var objs []*unstructured.Unstructured
	for _, n := range names {
		o, err := Load(filepath.Join(path, n))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		objs = append(objs, o...)
	}
	return objs, nil
}

// InPlace reports whether every object exists and a server-side dry-run apply would
// change none of them: an apply that changes nothing keeps the resourceVersion.
func InPlace(ctx context.Context, c ctrlclient.Client, objs []*unstructured.Unstructured, redact Redactor) (bool, error) {
	for _, obj := range objs {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		err := c.Get(ctx, ctrlclient.ObjectKeyFromObject(obj), live)
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		if err != nil {
			return false, describe(obj, err, redact)
		}
		dry := obj.DeepCopy()
		if err := applyOne(ctx, c, dry, ctrlclient.DryRunAll); err != nil {
			return false, describe(obj, err, redact)
		}
		if dry.GetResourceVersion() != live.GetResourceVersion() {
			return false, nil
		}
	}
	return true, nil
}

// Objects applies every object server-side, in order.
func Objects(ctx context.Context, c ctrlclient.Client, objs []*unstructured.Unstructured, redact Redactor) error {
	for _, obj := range objs {
		if err := applyOne(ctx, c, obj.DeepCopy()); err != nil {
			return describe(obj, err, redact)
		}
	}
	return nil
}

// WebhookWait bounds how long an apply waits for an admission webhook that isn't served
// yet: right after providers are installed, their Deployments can be Available before
// the webhook Service has endpoints.
var WebhookWait = 3 * time.Minute

// applyOne applies obj server-side, retrying while an admission webhook can't be
// reached. Every other error returns at once.
func applyOne(ctx context.Context, c ctrlclient.Client, obj *unstructured.Unstructured, opts ...ctrlclient.ApplyOption) error {
	opts = append([]ctrlclient.ApplyOption{ctrlclient.FieldOwner(FieldManager), ctrlclient.ForceOwnership}, opts...)
	deadline := time.Now().Add(WebhookWait)
	for {
		err := c.Apply(ctx, ctrlclient.ApplyConfigurationFromUnstructured(obj), opts...)
		if err == nil || !webhookUnreachable(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(3 * time.Second):
		}
	}
}

// webhookUnreachable: the API server couldn't call an admission webhook, as opposed to
// the webhook rejecting the object.
func webhookUnreachable(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "failed calling webhook") &&
		(strings.Contains(msg, "connection refused") || strings.Contains(msg, "no endpoints available") ||
			strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "context deadline exceeded") ||
			strings.Contains(msg, "x509:") || strings.Contains(msg, "EOF"))
}

// describe names the object in an error, then redacts all of it: object names can
// carry substituted values too.
func describe(obj *unstructured.Unstructured, err error, redact Redactor) error {
	return errors.New(redact(fmt.Sprintf("%s %s: %v", obj.GetKind(), obj.GetName(), err)))
}
