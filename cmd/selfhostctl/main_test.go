package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// stub is a phase with a fixed status; Run makes it done.
type stub struct {
	name string
	st   phase.Status
}

func (s *stub) Name() string                                 { return s.name }
func (s *stub) Status(context.Context) (phase.Status, error) { return s.st, nil }
func (s *stub) Run(context.Context) error                    { s.st = phase.Status{State: phase.Done}; return nil }

// testEnv is the operator's environment in these tests.
var testEnv = map[string]string{
	"FLUX_KEY":   "key",
	"BUNDLE":     `{"apiVersion":"selfhostctl/v1alpha1","kind":"RecoveryBundle","endpoint":{"host":"10.0.0.1","port":6443}}`,
	"KEY":        "a2V5",
	"DATASOURCE": "postgres://kine",
}

func testApp(st phase.Status) app {
	return app{
		lookupEnv: func(k string) (string, bool) { v, ok := testEnv[k]; return v, ok },
		phases: func(*config.ManagementCluster, bool) ([]phase.Phase, func(), error) {
			return []phase.Phase{&stub{name: "bootstrap-cluster", st: st}}, func() {}, nil
		},
		locate: func(_ context.Context, cfg *config.ManagementCluster) (string, string, bool, error) {
			// A config named "recovering" stands for a Recovery in progress.
			if cfg.Metadata.Name == "recovering" {
				return "bootstrap", "", true, nil
			}
			return "none", "", false, nil
		},
		recoverPhases: func(*config.ManagementCluster) ([]phase.Phase, func(), error) {
			return []phase.Phase{&stub{name: "outage", st: st}}, func() {}, nil
		},
		adopt: func(context.Context, *config.ManagementCluster) error { return nil },
		upgradePhases: func(*config.ManagementCluster) ([]phase.Phase, func(), error) {
			return []phase.Phase{&stub{name: "providers", st: st}}, func() {}, nil
		},
		gitopsPhases: func(*config.ManagementCluster) ([]phase.Phase, func(), error) {
			return []phase.Phase{&stub{name: "gitops", st: st}}, func() {}, nil
		},
		warnings: func(context.Context, *config.ManagementCluster) ([]string, error) {
			return []string{"Kustomization apps/web is suspended"}, nil
		},
		exportBundle: func(context.Context, *config.ManagementCluster) ([]byte, error) {
			return []byte("{\"kind\": \"RecoveryBundle\"}\n"), nil
		},
	}
}

func configFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "management.yaml")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// The cluster.manifests the configs name.
	if err := os.WriteFile(filepath.Join(dir, "cluster.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

const validConfig = "apiVersion: selfhostctl/v1alpha1\nkind: ManagementCluster\nmetadata:\n  name: management\nproviders:\n  bootstrap: [{name: k0sproject-k0smotron, version: v2.3.0}]\n  controlPlane: [{name: k0sproject-k0smotron, version: v2.3.0}]\n  infrastructure: [{name: docker, version: v1.14.4}]\ncluster:\n  manifests: cluster.yaml\n  namespace: mgmt\n"

const recoveryConfig = "recovery:\n  profile: k0smotron-kine\n  bundleFromEnv: BUNDLE\n  encryptionKeyFromEnv: KEY\n  kine: {datasourceFromEnv: DATASOURCE}\n"

// gitopsConfig names a known_hosts file configFile writes anyway.
const gitopsConfig = "gitops:\n  flux:\n    credentialSecret: {name: flux-git, namespace: flux-system, fromEnv: FLUX_KEY, knownHosts: cluster.yaml}\n"

func TestRun(t *testing.T) {
	cfg := configFile(t, validConfig)
	bad := configFile(t, validConfig+"kindConfig: kind.yaml\n")
	tests := []struct {
		name       string
		args       []string
		st         phase.Status
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"version"}, wantStdout: "dev\n"},
		{name: "--version", args: []string{"--version"}, wantStdout: "dev\n"},
		{name: "no command", args: nil, wantCode: exitUsage, wantStderr: "a command is required"},
		{name: "unknown command", args: []string{"pivot-now"}, wantCode: exitUsage},
		{name: "no config", args: []string{"bootstrap"}, wantCode: exitUsage, wantStderr: "--config/-f is required"},
		{name: "invalid config", args: []string{"bootstrap", "-f", bad}, wantCode: exitUsage, wantStderr: `unknown field "kindConfig"`},
		{name: "bootstrap runs", args: []string{"bootstrap", "-f", cfg}, st: phase.Status{State: phase.Next}, wantStdout: "bootstrap-cluster: running\nbootstrap-cluster: done\n"},
		{name: "bootstrap blocked", args: []string{"bootstrap", "-f", cfg}, st: phase.Status{State: phase.Blocked, Reason: "no Docker"}, wantCode: exitBlocked, wantStderr: "blocked: no Docker"},
		{name: "manual checkpoint", args: []string{"bootstrap", "-f", cfg}, st: phase.Status{State: phase.Manual, Reason: "add the deploy key"}, wantCode: exitCheckpoint, wantStderr: "manual checkpoint: add the deploy key"},
		{name: "unknown --from", args: []string{"bootstrap", "-f", cfg, "--from", "pivot"}, wantCode: exitUsage, wantStderr: `no phase "pivot"`},
		{name: "recovery export", args: []string{"recovery", "export", "-f", cfg}, wantStdout: "{\"kind\": \"RecoveryBundle\"}\n"},
		{name: "status", args: []string{"status", "-f", cfg}, st: phase.Status{State: phase.Next}, wantStdout: "location: none\nwarning: Kustomization apps/web is suspended\nbootstrap-cluster         NEXT\n"},
		{name: "gitops sync without gitops", args: []string{"gitops", "sync", "-f", cfg}, wantCode: exitUsage, wantStderr: "the config has no gitops"},
		{name: "gitops sync", args: []string{"gitops", "sync", "-f", configFile(t, validConfig+gitopsConfig)}, st: phase.Status{State: phase.Next}, wantStdout: "gitops: running\ngitops: done\n"},
		{name: "upgrade runs", args: []string{"upgrade", "-f", cfg}, st: phase.Status{State: phase.Next, Reason: "infrastructure-docker v1.14.3 → v1.14.4"},
			wantStdout: "providers: running (infrastructure-docker v1.14.3 → v1.14.4)\nproviders: done\n"},
		{name: "upgrade blocked", args: []string{"upgrade", "-f", cfg}, st: phase.Status{State: phase.Blocked, Reason: "a provider is missing"}, wantCode: exitBlocked, wantStderr: "blocked: a provider is missing"},
		{name: "providers adopt", args: []string{"providers", "adopt", "-f", cfg}, wantStdout: "providers adopted\n"},
		{name: "pivot starts at management-providers", args: []string{"pivot", "-f", cfg}, wantCode: exitUsage, wantStderr: `no phase "management-providers"`},
		{name: "recover without recovery", args: []string{"recover", "-f", cfg}, wantCode: exitUsage, wantStderr: "recover needs recovery in the config"},
		{name: "recover without its secrets", args: []string{"recover", "-f", configFile(t, validConfig+strings.Replace(recoveryConfig, "KEY", "UNSET_KEY", 1))},
			wantCode: exitUsage, wantStderr: "empty or unset: UNSET_KEY. Load them for this one command, e.g. op run -- selfhostctl recover"},
		{name: "recover runs", args: []string{"recover", "-f", configFile(t, validConfig+recoveryConfig)}, st: phase.Status{State: phase.Next}, wantStdout: "outage: running\noutage: done\n"},
		{name: "status during a Recovery", args: []string{"status", "-f", configFile(t, strings.Replace(validConfig, "name: management", "name: recovering", 1))}, st: phase.Status{State: phase.Done},
			wantStdout: "location: bootstrap, a Recovery is in progress\nwarning: Kustomization apps/web is suspended\noutage                    DONE\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(t.Context(), tt.args, &stdout, &stderr, testApp(tt.st)); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", got, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
