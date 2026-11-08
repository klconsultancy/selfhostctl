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
	BootstrapCluster BootstrapCluster `json:"bootstrapCluster"`
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
