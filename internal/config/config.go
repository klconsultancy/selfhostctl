// Package config loads and validates the declarative config of one Management cluster
// (apiVersion selfhostctl/v1alpha1, kind ManagementCluster). The schema is documented
// in docs/config.md.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/klconsultancy/selfhostctl/internal/envsubst"
)

const (
	APIVersion = "selfhostctl/v1alpha1"
	Kind       = "ManagementCluster"
)

// ManagementCluster is the whole config.
type ManagementCluster struct {
	APIVersion       string           `json:"apiVersion"`
	Kind             string           `json:"kind"`
	Metadata         Metadata         `json:"metadata"`
	Providers        Providers        `json:"providers"`
	BootstrapCluster BootstrapCluster `json:"bootstrapCluster"`
	// BeforeApply is installed before the cluster is applied, e.g. an API proxy the
	// infrastructure provider talks to, or a Secret the machines read at boot.
	BeforeApply []Prerequisite `json:"beforeApply,omitempty"`
	// AfterPivot is installed on the Management cluster once it manages itself, before
	// the Bootstrap cluster goes. Entries take no clusters.
	AfterPivot []Prerequisite `json:"afterPivot,omitempty"`
	// Cluster is the Management cluster's own definition.
	Cluster Cluster `json:"cluster"`
	// Recovery is how the Management cluster is recovered when its machines are gone.
	// Without it, bootstrap doesn't ask for a Recovery bundle.
	Recovery *Recovery `json:"recovery,omitempty"`
	// GitOps is how the Management cluster's GitOps controller gets its credential.
	// Without it, bootstrap ends without touching GitOps.
	GitOps *GitOps `json:"gitops,omitempty"`

	// dir is the config file's directory; relative paths in the config are relative to
	// it.
	dir string
}

// Cluster is the Management cluster's definition and where its objects live.
type Cluster struct {
	// Manifests is a kustomization directory, a directory of YAML files, or one file:
	// the Cluster, its control plane, infrastructure and machine templates.
	Manifests string `json:"manifests"`
	// Namespace of the Cluster object: the one namespace a move covers.
	Namespace string `json:"namespace"`
	// Name of the Cluster object; metadata.name when empty.
	Name string `json:"name,omitempty"`
	// Kubeconfig is where bootstrap writes the Management cluster's admin kubeconfig;
	// ~/.kube/<metadata.name>.kubeconfig when empty.
	Kubeconfig string `json:"kubeconfig,omitempty"`
}

// GitOps names the GitOps controller. Flux is the one there is.
type GitOps struct {
	Flux *Flux `json:"flux"`
}

// Flux comes from the cluster's own definition; the tool only gives it its git
// credential and checks that it syncs.
type Flux struct {
	CredentialSecret CredentialSecret `json:"credentialSecret"`
	// Root is the Kustomization that syncs everything else, flux-operator's
	// flux-system/flux-system when empty.
	Root *ObjectRef `json:"root,omitempty"`
}

// CredentialSecret is the Secret Flux's source reads: identity from the environment,
// known_hosts from a file.
type CredentialSecret struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// FromEnv is the variable holding the SSH private key (identity).
	FromEnv string `json:"fromEnv" selfhostctl:"env"`
	// KnownHosts is a file with the git server's host keys, in known_hosts format.
	KnownHosts string `json:"knownHosts"`
}

// ObjectRef names a namespaced object.
type ObjectRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// FluxRoot is the root Kustomization, with its default.
func (f *Flux) FluxRoot() ObjectRef {
	if f.Root != nil {
		return *f.Root
	}
	return ObjectRef{Namespace: "flux-system", Name: "flux-system"}
}

// ProfileK0smotronKine is the one recovery profile the tool has.
const ProfileK0smotronKine = "k0smotron-kine"

// Recovery names the recovery profile and the environment variables a Recovery reads.
// None of them is required to start a bootstrap: the bootstrap's recovery-bundle
// checkpoint checks them before it finishes.
type Recovery struct {
	Profile string `json:"profile"`
	// BundleFromEnv is the variable holding the Recovery bundle, as `selfhostctl
	// recovery export` writes it.
	BundleFromEnv string `json:"bundleFromEnv"`
	// EncryptionKeyFromEnv is the variable holding the key Secrets are encrypted with
	// at rest in the datastore.
	EncryptionKeyFromEnv string `json:"encryptionKeyFromEnv"`
	Kine                 *Kine  `json:"kine,omitempty"`
}

type Kine struct {
	// DatasourceFromEnv is the variable holding kine's data source, e.g.
	// postgres://user:password@host:5432/db.
	DatasourceFromEnv string `json:"datasourceFromEnv"`
}

// ClusterName is the Management cluster's Cluster object's name.
func (c *ManagementCluster) ClusterName() string {
	if c.Cluster.Name != "" {
		return c.Cluster.Name
	}
	return c.Metadata.Name
}

// KubeconfigPath is where bootstrap writes the admin kubeconfig, ~ expanded.
func (c *ManagementCluster) KubeconfigPath() (string, error) {
	p := c.Cluster.Kubeconfig
	if p == "" {
		p = "~/.kube/" + c.Metadata.Name + ".kubeconfig"
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, rest), nil
	}
	return c.Path(p), nil
}

// The clusters a Prerequisite can be installed on.
const (
	OnBootstrap  = "bootstrap"
	OnManagement = "management"
)

// Prerequisite is one manifest file or one Helm chart.
type Prerequisite struct {
	Manifest *Manifest `json:"manifest,omitempty"`
	Helm     *Helm     `json:"helm,omitempty"`
	// Clusters lists where it is installed: the Bootstrap cluster, the Management
	// cluster itself, or both. Not `on`: YAML 1.1 reads that key as the boolean true.
	Clusters []string `json:"clusters"`
}

// Manifest is a YAML file of Kubernetes objects, with ${VAR} substituted from the
// environment, applied server-side.
type Manifest struct {
	Path string `json:"path"`
}

// Helm is a chart installed or upgraded through the Helm SDK.
type Helm struct {
	// Chart is an oci:// reference, a chart name in Repo, or a local directory.
	Chart string `json:"chart"`
	// Repo is a classic chart repository's URL, for a chart name.
	Repo    string `json:"repo,omitempty"`
	Version string `json:"version,omitempty"`
	Release string `json:"release"`
	// Namespace of the release, created when missing.
	Namespace string `json:"namespace"`
	// Values is a YAML file, with ${VAR} substituted from the environment.
	Values string `json:"values,omitempty"`
}

// Path resolves a path from the config relative to the config file.
func (c *ManagementCluster) Path(p string) string {
	if p == "" || path.IsAbs(p) || c.dir == "" {
		return p
	}
	return path.Join(c.dir, p)
}

// IsOn reports whether p is installed on the given cluster (OnBootstrap or
// OnManagement).
func (p Prerequisite) IsOn(cluster string) bool { return slices.Contains(p.Clusters, cluster) }

// Name describes p in output.
func (p Prerequisite) Name() string {
	if p.Manifest != nil {
		return "manifest " + p.Manifest.Path
	}
	if p.Helm != nil {
		return "chart " + p.Helm.Release
	}
	return "nothing"
}

// Providers are the Cluster API providers besides the core, whose version follows from
// the tool release. Versions stay plain scalars so external tools (Renovate, drift
// alerts) can read them.
type Providers struct {
	Bootstrap      []Provider `json:"bootstrap"`
	ControlPlane   []Provider `json:"controlPlane"`
	Infrastructure []Provider `json:"infrastructure"`
	IPAM           []Provider `json:"ipam,omitempty"`
	Addon          []Provider `json:"addon,omitempty"`
	// ClusterTopology turns on Cluster API's ClusterTopology feature gate where the
	// core still needs it (CAPI < 1.15); from 1.15 it is on by default.
	ClusterTopology bool `json:"clusterTopology,omitempty"`
	// Variables are clusterctl variables for the providers' components, e.g. other
	// feature gates. They are the only ones used: the operator's environment never
	// reaches clusterctl.
	Variables map[string]string `json:"variables,omitempty"`
}

type Provider struct {
	// Name as clusterctl knows it, e.g. k0sproject-k0smotron.
	Name    string `json:"name"`
	Version string `json:"version"`
	// URL of the components for a provider clusterctl doesn't know, or a fork.
	URL string `json:"url,omitempty"`
}

type Metadata struct {
	// Name of the Management cluster. It names the tool's own objects, such as the
	// Bootstrap cluster's container.
	Name string `json:"name"`
}

// BootstrapCluster is what the Bootstrap cluster needs from its host.
type BootstrapCluster struct {
	// Network is the Docker network the Bootstrap cluster joins, e.g. the one an
	// infrastructure provider in Docker puts its machines on. Empty: Docker's default.
	Network string `json:"network,omitempty"`
	// ExtraMounts are bind mounts from the host, e.g. the Docker socket for CAPD.
	ExtraMounts []Mount `json:"extraMounts,omitempty"`
	// DeleteAfterStable is how long the Management cluster must manage itself, with no
	// machine replaced, before the Bootstrap cluster is deleted. Default 10m.
	DeleteAfterStable string `json:"deleteAfterStable,omitempty"`
}

// StableWindow is DeleteAfterStable as a duration.
func (b BootstrapCluster) StableWindow() time.Duration {
	d, err := time.ParseDuration(b.DeleteAfterStable)
	if err != nil || b.DeleteAfterStable == "" {
		return 10 * time.Minute
	}
	return d
}

type Mount struct {
	HostPath      string `json:"hostPath"`
	ContainerPath string `json:"containerPath"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
}

// Error is a config that can't be used: unreadable, malformed or invalid. Every
// problem found is listed.
type Error struct {
	File     string
	Problems []string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.File, strings.Join(e.Problems, "; "))
}

// Load reads, decodes and validates the config at file.
func Load(file string) (*ManagementCluster, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, &Error{File: file, Problems: []string{err.Error()}}
	}
	c, err := Parse(file, data)
	if err != nil {
		return nil, err
	}
	c.dir = filepath.Dir(file)
	if problems := c.checkFiles(); len(problems) > 0 {
		return nil, &Error{File: file, Problems: problems}
	}
	return c, nil
}

// checkFiles: every file the config names exists.
func (c *ManagementCluster) checkFiles() []string {
	var problems []string
	if _, err := os.Stat(c.Path(c.Cluster.Manifests)); err != nil {
		problems = append(problems, fmt.Sprintf("cluster.manifests: %v", err))
	}
	if g := c.GitOps; g != nil && g.Flux != nil {
		if _, err := os.Stat(c.Path(g.Flux.CredentialSecret.KnownHosts)); err != nil {
			problems = append(problems, fmt.Sprintf("gitops.flux.credentialSecret.knownHosts: %v", err))
		}
	}
	entries := map[string]Prerequisite{}
	for i, p := range c.BeforeApply {
		entries[fmt.Sprintf("beforeApply[%d]", i)] = p
	}
	for i, p := range c.AfterPivot {
		entries[fmt.Sprintf("afterPivot[%d]", i)] = p
	}
	for _, at := range slices.Sorted(maps.Keys(entries)) {
		p := entries[at]
		var files []string
		if p.Manifest != nil {
			files = append(files, p.Manifest.Path)
		}
		if p.Helm != nil && p.Helm.Values != "" {
			files = append(files, p.Helm.Values)
		}
		for _, f := range files {
			if _, err := os.Stat(c.Path(f)); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", at, err))
			}
		}
	}
	return problems
}

// FileEnvRefs lists the environment variables the files the config names refer to
// as ${VAR}: manifests and Helm values.
func (c *ManagementCluster) FileEnvRefs() ([]string, error) {
	var names []string
	for _, p := range append(slices.Clone(c.BeforeApply), c.AfterPivot...) {
		var file string
		switch {
		case p.Manifest != nil:
			file = p.Manifest.Path
		case p.Helm != nil && p.Helm.Values != "":
			file = p.Helm.Values
		default:
			continue
		}
		data, err := os.ReadFile(c.Path(file))
		if err != nil {
			return nil, &Error{File: file, Problems: []string{err.Error()}}
		}
		names = append(names, envsubst.Refs(string(data))...)
	}
	return names, nil
}

// Parse decodes and validates a config. file only names it in errors.
func Parse(file string, data []byte) (*ManagementCluster, error) {
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, &Error{File: file, Problems: []string{err.Error()}}
	}
	var raw any
	if err := json.Unmarshal(jsonData, &raw); err != nil {
		return nil, &Error{File: file, Problems: []string{err.Error()}}
	}
	if problems := unknownFields(raw, reflect.TypeFor[ManagementCluster](), ""); len(problems) > 0 {
		return nil, &Error{File: file, Problems: problems}
	}
	var c ManagementCluster
	if err := json.Unmarshal(jsonData, &c); err != nil {
		return nil, &Error{File: file, Problems: []string{err.Error()}}
	}
	if problems := c.validate(); len(problems) > 0 {
		return nil, &Error{File: file, Problems: problems}
	}
	return &c, nil
}

// unknownFields lists every key in raw that t doesn't declare, by its full path.
func unknownFields(raw any, t reflect.Type, at string) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var problems []string
	switch t.Kind() {
	case reflect.Struct:
		m, ok := raw.(map[string]any)
		if !ok {
			return nil // a type mismatch, reported by the decoder
		}
		fields := map[string]reflect.Type{}
		for f := range t.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name != "" && name != "-" {
				fields[name] = f.Type
			}
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			p := k
			if at != "" {
				p = at + "." + k
			}
			ft, known := fields[k]
			if !known {
				problems = append(problems, fmt.Sprintf("unknown field %q", p))
				continue
			}
			problems = append(problems, unknownFields(m[k], ft, p)...)
		}
	case reflect.Slice:
		items, ok := raw.([]any)
		if !ok {
			return nil
		}
		for i, item := range items {
			problems = append(problems, unknownFields(item, t.Elem(), fmt.Sprintf("%s[%d]", at, i))...)
		}
	}
	return problems
}

// A DNS-1123 label: it names Docker containers and Kubernetes objects.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func (c *ManagementCluster) validate() []string {
	var problems []string
	if c.APIVersion != APIVersion {
		problems = append(problems, fmt.Sprintf("apiVersion: %q, want %q", c.APIVersion, APIVersion))
	}
	if c.Kind != Kind {
		problems = append(problems, fmt.Sprintf("kind: %q, want %q", c.Kind, Kind))
	}
	if !dnsLabel.MatchString(c.Metadata.Name) || len(c.Metadata.Name) > 40 {
		problems = append(problems, fmt.Sprintf("metadata.name: %q must be a lowercase DNS label of at most 40 characters", c.Metadata.Name))
	}
	problems = append(problems, c.Providers.validate()...)
	if c.Cluster.Manifests == "" {
		problems = append(problems, "cluster.manifests: required")
	}
	if !dnsLabel.MatchString(c.Cluster.Namespace) {
		problems = append(problems, fmt.Sprintf("cluster.namespace: %q must be a lowercase DNS label", c.Cluster.Namespace))
	}
	if c.Cluster.Name != "" && !dnsLabel.MatchString(c.Cluster.Name) {
		problems = append(problems, fmt.Sprintf("cluster.name: %q must be a lowercase DNS label", c.Cluster.Name))
	}
	if r := c.Recovery; r != nil {
		if r.Profile != ProfileK0smotronKine {
			problems = append(problems, fmt.Sprintf("recovery.profile: %q, the tool has only %q", r.Profile, ProfileK0smotronKine))
		}
		names := map[string]string{"recovery.bundleFromEnv": r.BundleFromEnv, "recovery.encryptionKeyFromEnv": r.EncryptionKeyFromEnv}
		if r.Kine == nil {
			problems = append(problems, "recovery.kine: required for profile "+ProfileK0smotronKine)
		} else {
			names["recovery.kine.datasourceFromEnv"] = r.Kine.DatasourceFromEnv
		}
		for _, field := range slices.Sorted(maps.Keys(names)) {
			if !envName.MatchString(names[field]) {
				problems = append(problems, fmt.Sprintf("%s: %q is not an environment variable name", field, names[field]))
			}
		}
	}
	problems = append(problems, c.GitOps.validate()...)
	for i, p := range c.BeforeApply {
		problems = append(problems, p.validate(fmt.Sprintf("beforeApply[%d]", i))...)
	}
	for i, p := range c.AfterPivot {
		at := fmt.Sprintf("afterPivot[%d]", i)
		if len(p.Clusters) > 0 {
			problems = append(problems, at+".clusters: not taken; afterPivot is for the Management cluster")
		}
		p.Clusters = []string{OnManagement}
		problems = append(problems, p.validate(at)...)
	}
	if b := c.BootstrapCluster.DeleteAfterStable; b != "" {
		if d, err := time.ParseDuration(b); err != nil || d < 0 {
			problems = append(problems, fmt.Sprintf("bootstrapCluster.deleteAfterStable: %q is not a duration like 10m", b))
		}
	}
	for i, m := range c.BootstrapCluster.ExtraMounts {
		at := fmt.Sprintf("bootstrapCluster.extraMounts[%d]", i)
		if !path.IsAbs(m.HostPath) {
			problems = append(problems, at+".hostPath: must be an absolute path")
		}
		if !path.IsAbs(m.ContainerPath) {
			problems = append(problems, at+".containerPath: must be an absolute path")
		}
	}
	return problems
}

func (p Prerequisite) validate(at string) []string {
	var problems []string
	if (p.Manifest == nil) == (p.Helm == nil) {
		problems = append(problems, at+": exactly one of manifest and helm is required")
	}
	if len(p.Clusters) == 0 {
		problems = append(problems, at+".clusters: at least one of bootstrap, management is required")
	}
	for _, o := range p.Clusters {
		if o != OnBootstrap && o != OnManagement {
			problems = append(problems, fmt.Sprintf("%s.clusters: %q is neither bootstrap nor management", at, o))
		}
	}
	if m := p.Manifest; m != nil && m.Path == "" {
		problems = append(problems, at+".manifest.path: required")
	}
	if h := p.Helm; h != nil {
		if h.Chart == "" {
			problems = append(problems, at+".helm.chart: required")
		}
		if !dnsLabel.MatchString(h.Release) {
			problems = append(problems, fmt.Sprintf("%s.helm.release: %q must be a lowercase DNS label", at, h.Release))
		}
		if !dnsLabel.MatchString(h.Namespace) {
			problems = append(problems, fmt.Sprintf("%s.helm.namespace: %q must be a lowercase DNS label", at, h.Namespace))
		}
		remote := strings.HasPrefix(h.Chart, "oci://") || h.Repo != ""
		if remote && h.Version == "" {
			problems = append(problems, at+".helm.version: required for a remote chart, so the install is reproducible")
		}
	}
	return problems
}

func (g *GitOps) validate() []string {
	if g == nil {
		return nil
	}
	if g.Flux == nil {
		return []string{"gitops.flux: required with gitops"}
	}
	var problems []string
	s := g.Flux.CredentialSecret
	if !dnsSubdomain.MatchString(s.Name) {
		problems = append(problems, fmt.Sprintf("gitops.flux.credentialSecret.name: %q must be a lowercase DNS name", s.Name))
	}
	if !dnsLabel.MatchString(s.Namespace) {
		problems = append(problems, fmt.Sprintf("gitops.flux.credentialSecret.namespace: %q must be a lowercase DNS label", s.Namespace))
	}
	if !envName.MatchString(s.FromEnv) {
		problems = append(problems, fmt.Sprintf("gitops.flux.credentialSecret.fromEnv: %q is not an environment variable name", s.FromEnv))
	}
	if s.KnownHosts == "" {
		problems = append(problems, "gitops.flux.credentialSecret.knownHosts: required: the git server's host keys")
	}
	if r := g.Flux.Root; r != nil {
		if !dnsLabel.MatchString(r.Namespace) {
			problems = append(problems, fmt.Sprintf("gitops.flux.root.namespace: %q must be a lowercase DNS label", r.Namespace))
		}
		if !dnsSubdomain.MatchString(r.Name) {
			problems = append(problems, fmt.Sprintf("gitops.flux.root.name: %q must be a lowercase DNS name", r.Name))
		}
	}
	return problems
}

// A DNS-1123 subdomain: most Kubernetes object names.
var dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var providerVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]+)?$`)

func (p *Providers) validate() []string {
	var problems []string
	lists := []struct {
		field    string
		list     []Provider
		required bool
	}{
		// clusterctl adds kubeadm's latest release for a bootstrap or control-plane
		// type left empty, so those must be declared.
		{"providers.bootstrap", p.Bootstrap, true},
		{"providers.controlPlane", p.ControlPlane, true},
		{"providers.infrastructure", p.Infrastructure, true},
		{"providers.ipam", p.IPAM, false},
		{"providers.addon", p.Addon, false},
	}
	for _, l := range lists {
		if l.required && len(l.list) == 0 {
			problems = append(problems, l.field+": at least one provider is required")
		}
		seen := map[string]bool{}
		for i, pr := range l.list {
			at := fmt.Sprintf("%s[%d]", l.field, i)
			if !dnsLabel.MatchString(pr.Name) {
				problems = append(problems, fmt.Sprintf("%s.name: %q is not a provider name", at, pr.Name))
			}
			if seen[pr.Name] {
				problems = append(problems, fmt.Sprintf("%s.name: %q is declared twice", at, pr.Name))
			}
			seen[pr.Name] = true
			if !providerVersion.MatchString(pr.Version) {
				problems = append(problems, fmt.Sprintf("%s.version: %q is not a version like v1.2.3", at, pr.Version))
			}
			if pr.URL != "" && !strings.HasPrefix(pr.URL, "https://") {
				problems = append(problems, at+".url: must be an https URL")
			}
		}
	}
	for k := range p.Variables {
		if k == "CLUSTER_TOPOLOGY" {
			problems = append(problems, "providers.variables: set CLUSTER_TOPOLOGY through providers.clusterTopology")
		}
	}
	return problems
}

// MissingEnvError names every environment variable the config needs that is unset or
// empty.
type MissingEnvError struct {
	Names []string
}

func (e *MissingEnvError) Error() string {
	return "unset or empty environment variables the config needs: " + strings.Join(e.Names, ", ")
}

// CheckEnv returns a *MissingEnvError when any variable the config needs is unset or
// empty, before anything runs: the config's env references and the ${VAR}s in the files
// it names. lookup is os.LookupEnv outside tests.
func (c *ManagementCluster) CheckEnv(lookup func(string) (string, bool)) error {
	fileRefs, err := c.FileEnvRefs()
	if err != nil {
		return err
	}
	var missing []string
	for _, name := range append(envRefs(reflect.ValueOf(c)), fileRefs...) {
		if v, ok := lookup(name); !ok || v == "" {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	missing = slices.Compact(missing)
	if len(missing) > 0 {
		return &MissingEnvError{Names: missing}
	}
	return nil
}

// envTag marks a string field whose value is the name of a required environment
// variable, e.g. `json:"fromEnv" selfhostctl:"env"`.
const envTag = "env"

// envRefs collects the values of every non-empty string field tagged
// `selfhostctl:"env"`, anywhere in v.
func envRefs(v reflect.Value) []string {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	var names []string
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			if f.Tag.Get("selfhostctl") == envTag && f.Type.Kind() == reflect.String {
				if s := v.Field(i).String(); s != "" {
					names = append(names, s)
				}
				continue
			}
			names = append(names, envRefs(v.Field(i))...)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			names = append(names, envRefs(v.Index(i))...)
		}
	}
	return names
}

// IsConfigError reports whether err comes from loading or checking the config.
func IsConfigError(err error) bool {
	var ce *Error
	var me *MissingEnvError
	return errors.As(err, &ce) || errors.As(err, &me)
}
