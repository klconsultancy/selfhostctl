// Package config loads and validates the declarative config of one Management cluster
// (apiVersion selfhostctl/v1alpha1, kind ManagementCluster). The schema is documented
// in docs/config.md.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
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
	return Parse(file, data)
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
// empty, before anything runs. lookup is os.LookupEnv outside tests.
func (c *ManagementCluster) CheckEnv(lookup func(string) (string, bool)) error {
	var missing []string
	for _, name := range envRefs(reflect.ValueOf(c)) {
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
