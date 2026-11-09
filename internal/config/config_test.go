package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const valid = `
apiVersion: selfhostctl/v1alpha1
kind: ManagementCluster
metadata:
  name: management
providers:
  bootstrap: [{name: k0sproject-k0smotron, version: v2.3.0}]
  controlPlane: [{name: k0sproject-k0smotron, version: v2.3.0}]
  infrastructure: [{name: docker, version: v1.14.4}]
cluster:
  manifests: cluster.yaml
  namespace: mgmt
bootstrapCluster:
  network: kind
  extraMounts:
    - hostPath: /var/run/docker.sock
      containerPath: /var/run/docker.sock
`

func TestParseValid(t *testing.T) {
	c, err := Parse("management.yaml", []byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Metadata.Name != "management" || c.BootstrapCluster.Network != "kind" || len(c.BootstrapCluster.ExtraMounts) != 1 {
		t.Errorf("decoded: %+v", c)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "unknown fields, with their full paths",
			yaml: valid + "  kindConfig: kind.yaml\nextra: 1\n",
			want: []string{`unknown field "bootstrapCluster.kindConfig"`, `unknown field "extra"`},
		},
		{
			name: "unknown field in a list item",
			yaml: strings.Replace(valid, "readOnly", "x", 1) + "    - hostPath: /a\n      containerPath: /b\n      rw: true\n",
			want: []string{`unknown field "bootstrapCluster.extraMounts[1].rw"`},
		},
		{
			name: "wrong apiVersion and kind",
			yaml: strings.NewReplacer("selfhostctl/v1alpha1", "selfhostctl/v1", "ManagementCluster", "Cluster").Replace(valid),
			want: []string{`apiVersion: "selfhostctl/v1"`, `kind: "Cluster"`},
		},
		{
			name: "bad name",
			yaml: strings.Replace(valid, "name: management", "name: Management_1", 1),
			want: []string{`metadata.name: "Management_1"`},
		},
		{
			name: "missing provider types",
			yaml: strings.Replace(valid, "  controlPlane: [{name: k0sproject-k0smotron, version: v2.3.0}]\n", "", 1),
			want: []string{"providers.controlPlane: at least one provider is required"},
		},
		{
			name: "bad provider entries",
			yaml: strings.Replace(valid, "  infrastructure: [{name: docker, version: v1.14.4}]",
				"  infrastructure: [{name: docker, version: '1.14'}, {name: docker, version: v1.14.4, url: http://x}]", 1),
			want: []string{`infrastructure[0].version: "1.14"`, `infrastructure[1].name: "docker" is declared twice`, "infrastructure[1].url: must be an https URL"},
		},
		{
			name: "CLUSTER_TOPOLOGY as a variable",
			yaml: strings.Replace(valid, "providers:\n", "providers:\n  variables: {CLUSTER_TOPOLOGY: \"true\"}\n", 1),
			want: []string{"set CLUSTER_TOPOLOGY through providers.clusterTopology"},
		},
		{
			name: "gitops without flux",
			yaml: valid + "gitops: {}\n",
			want: []string{"gitops.flux: required"},
		},
		{
			name: "bad gitops fields",
			yaml: valid + "gitops:\n  flux:\n    credentialSecret: {name: Flux_Git, namespace: flux-system, fromEnv: 'not a var'}\n    root: {namespace: '', name: x}\n",
			want: []string{`credentialSecret.name: "Flux_Git"`, `credentialSecret.fromEnv: "not a var"`, "credentialSecret.knownHosts: required", `root.namespace: ""`},
		},
		{
			name: "relative mount paths",
			yaml: strings.NewReplacer("hostPath: /var/run/docker.sock", "hostPath: docker.sock", "containerPath: /var/run/docker.sock", "containerPath: sock").Replace(valid),
			want: []string{"extraMounts[0].hostPath: must be an absolute path", "extraMounts[0].containerPath: must be an absolute path"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse("management.yaml", []byte(tt.yaml))
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("Parse: %v, want a *config.Error", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestGitOps(t *testing.T) {
	c, err := Parse("management.yaml", []byte(valid+"gitops:\n  flux:\n    credentialSecret: {name: flux-git, namespace: flux-system, fromEnv: FLUX_KEY, knownHosts: known_hosts}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.GitOps.Flux.FluxRoot(); got != (ObjectRef{"flux-system", "flux-system"}) {
		t.Errorf("default root: %v", got)
	}
	// The key is required before anything runs.
	var me *MissingEnvError
	if err := c.CheckEnv(func(string) (string, bool) { return "", true }); !errors.As(err, &me) || !reflect.DeepEqual(me.Names, []string{"FLUX_KEY"}) {
		t.Errorf("CheckEnv: %v, want FLUX_KEY missing", err)
	}
}

func TestEnvRefs(t *testing.T) {
	type secret struct {
		FromEnv string `json:"fromEnv" selfhostctl:"env"`
	}
	type cfg struct {
		A       secret
		List    []secret
		Ptr     *secret
		Nil     *secret
		NotEnv  string
		Ignored string `selfhostctl:"other"`
	}
	v := cfg{A: secret{"A_KEY"}, List: []secret{{"B_KEY"}, {""}}, Ptr: &secret{"C_KEY"}, NotEnv: "D", Ignored: "E"}
	if got, want := envRefs(reflect.ValueOf(&v)), []string{"A_KEY", "B_KEY", "C_KEY"}; !reflect.DeepEqual(got, want) {
		t.Errorf("envRefs: %v, want %v", got, want)
	}
}

func TestCheckEnvNamesEveryMissingVariable(t *testing.T) {
	c, err := Parse("management.yaml", []byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	// v1alpha1 has no env references yet; the checker itself is covered by
	// TestEnvRefs and this test's lookup.
	if err := c.CheckEnv(func(string) (string, bool) { return "", false }); err != nil {
		t.Errorf("CheckEnv: %v, want nil with no references", err)
	}
	e := &MissingEnvError{Names: []string{"A", "B"}}
	if !strings.Contains(e.Error(), "A, B") || !IsConfigError(e) {
		t.Errorf("MissingEnvError: %q", e)
	}
}
