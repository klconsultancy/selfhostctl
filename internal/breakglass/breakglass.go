// Package breakglass derives the Break-glass controller (see GLOSSARY.md) of recovery
// profile k0smotron-kine from the Management cluster's K0sControlPlane
// (docs/design.md#recovery).
package breakglass

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/klconsultancy/selfhostctl/internal/recovery"
)

// K0sControlPlaneGVK is k0smotron's control plane.
var K0sControlPlaneGVK = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta2", Kind: "K0sControlPlane"}

// ConfigPath is where the controller's k0s config is written.
const ConfigPath = "/etc/k0s/k0s.yaml"

// Secrets are what the controller needs from the environment.
type Secrets struct {
	Bundle *recovery.Bundle
	// Datasource is kine's data source.
	Datasource string
	// EncryptionKey is the aescbc key the Secrets are encrypted with at rest.
	EncryptionKey string
}

// Controller is a Break-glass controller: what its container runs and the files it
// starts with.
type Controller struct {
	// Version is k0s's, e.g. v1.36.5+k0s.0.
	Version string
	// Args follow `k0s`.
	Args []string
	// Files are written into the container before it starts, by absolute path, mode
	// 0600; Readable ones 0644.
	Files    map[string][]byte
	Readable map[string]bool
}

// Image is the k0s image of a k0s version: its "+" becomes "-".
func Image(version string) string {
	return "docker.io/k0sproject/k0s:" + strings.ReplaceAll(version, "+", "-")
}

// Find returns the K0sControlPlane the Cluster namespace/name refers to, among objs.
func Find(objs []*unstructured.Unstructured, namespace, name string) (*unstructured.Unstructured, error) {
	var ref map[string]any
	for _, o := range objs {
		if o.GetKind() == "Cluster" && o.GroupVersionKind().Group == "cluster.x-k8s.io" && o.GetNamespace() == namespace && o.GetName() == name {
			ref, _, _ = unstructured.NestedMap(o.Object, "spec", "controlPlaneRef")
		}
	}
	if ref == nil {
		return nil, fmt.Errorf("no Cluster %s/%s with a controlPlaneRef in cluster.manifests", namespace, name)
	}
	if ref["kind"] != K0sControlPlaneGVK.Kind {
		return nil, fmt.Errorf("the control plane of Cluster %s/%s is a %v; recovery profile k0smotron-kine needs a K0sControlPlane", namespace, name, ref["kind"])
	}
	for _, o := range objs {
		if o.GetKind() == K0sControlPlaneGVK.Kind && o.GetNamespace() == namespace && o.GetName() == ref["name"] {
			return o, nil
		}
	}
	return nil, fmt.Errorf("no K0sControlPlane %s/%v in cluster.manifests", namespace, ref["name"])
}

// Derive builds the controller from a K0sControlPlane, at version (the manifest's when
// empty).
func Derive(kcp *unstructured.Unstructured, version string, s Secrets) (Controller, error) {
	if version == "" {
		version, _, _ = unstructured.NestedString(kcp.Object, "spec", "version")
	}
	if version == "" {
		return Controller{}, errors.New("the K0sControlPlane has no spec.version")
	}
	if s.Bundle == nil || s.Bundle.Endpoint.Host == "" {
		return Controller{}, errors.New("the Recovery bundle has no endpoint")
	}
	if s.Datasource == "" {
		return Controller{}, errors.New("no kine data source")
	}
	spec, _, _ := unstructured.NestedMap(kcp.Object, "spec", "k0sConfigSpec", "k0s", "spec")

	storage, _, _ := unstructured.NestedMap(spec, "storage")
	if storage == nil || storage["type"] != "kine" {
		return Controller{}, fmt.Errorf("recovery profile k0smotron-kine needs storage.type kine in the K0sControlPlane, not %v", storage["type"])
	}
	if err := unstructured.SetNestedField(storage, s.Datasource, "kine", "dataSource"); err != nil {
		return Controller{}, err
	}

	api := map[string]any{
		"externalAddress": s.Bundle.Endpoint.Host,
		"sans":            []any{"127.0.0.1", "localhost"},
	}
	files := map[string][]byte{}
	readable := map[string]bool{}
	if extra, ok, _ := unstructured.NestedMap(spec, "api", "extraArgs"); ok {
		api["extraArgs"] = extra
		if path, _ := extra["encryption-provider-config"].(string); path != "" {
			if s.EncryptionKey == "" {
				return Controller{}, errors.New("the K0sControlPlane encrypts Secrets at rest, and there is no encryption key")
			}
			files[path] = encryptionConfig(s.EncryptionKey)
			// kube-apiserver reads it, and k0s runs it as its own user: 0644, as the
			// machines have it.
			readable[path] = true
		}
	}

	local := map[string]any{"api": api, "storage": storage}
	network := map[string]any{}
	for _, f := range []string{"podCIDR", "serviceCIDR"} {
		if v, ok, _ := unstructured.NestedString(spec, "network", f); ok {
			network[f] = v
		}
	}
	if len(network) > 0 {
		local["network"] = network
	}
	cfg, err := yaml.Marshal(map[string]any{
		"apiVersion": "k0s.k0sproject.io/v1beta1",
		"kind":       "ClusterConfig",
		"metadata":   map[string]any{"name": "k0s"},
		"spec":       local,
	})
	if err != nil {
		return Controller{}, err
	}
	files[ConfigPath] = cfg
	for path, content := range pki(s.Bundle) {
		files[path] = content
		// Certificates are public, and kube-apiserver reads them as its own user:
		// 0644, as k0s writes them. k0s itself chowns the keys its components read.
		if !strings.HasSuffix(path, ".key") {
			readable[path] = true
		}
	}

	args, _, _ := unstructured.NestedStringSlice(kcp.Object, "spec", "k0sConfigSpec", "args")
	return Controller{
		Version: version,
		Args: []string{"controller", "--config=" + ConfigPath, "--enable-dynamic-config",
			"--disable-components=" + strings.Join(disabled(args), ",")},
		Files:    files,
		Readable: readable,
	}, nil
}

// disabled is konnectivity-server plus the components the args disable: the
// controller has no worker for konnectivity agents to reach it through.
func disabled(args []string) []string {
	out := []string{"konnectivity-server"}
	for i, a := range args {
		var list string
		switch {
		case strings.HasPrefix(a, "--disable-components="):
			list = strings.TrimPrefix(a, "--disable-components=")
		case a == "--disable-components" && i+1 < len(args):
			list = args[i+1]
		default:
			continue
		}
		for c := range strings.SplitSeq(list, ",") {
			if c = strings.TrimSpace(c); c != "" && !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

func encryptionConfig(key string) []byte {
	return fmt.Appendf(nil, `apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - aescbc:
          keys:
            - name: key1
              secret: %q
      - identity: {}
`, key)
}

// pki is the bundle's key pairs where k0s looks for them.
func pki(b *recovery.Bundle) map[string][]byte {
	dir := "/var/lib/k0s/pki/"
	return map[string][]byte{
		dir + "ca.crt":             []byte(b.CA.Cert),
		dir + "ca.key":             []byte(b.CA.Key),
		dir + "sa.pub":             []byte(b.ServiceAccount.Cert),
		dir + "sa.key":             []byte(b.ServiceAccount.Key),
		dir + "front-proxy-ca.crt": []byte(b.FrontProxyCA.Cert),
		dir + "front-proxy-ca.key": []byte(b.FrontProxyCA.Key),
	}
}
