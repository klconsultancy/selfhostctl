package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeforeApplyValidation(t *testing.T) {
	tests := []struct {
		name  string
		entry string
		want  string
	}{
		{"neither", "  - clusters: [bootstrap]\n", "exactly one of manifest and helm"},
		{"both", "  - manifest: {path: a.yaml}\n    helm: {chart: ./c, release: proxy, namespace: proxy}\n    clusters: [bootstrap]\n", "exactly one of manifest and helm"},
		{"no clusters", "  - manifest: {path: a.yaml}\n", "at least one of bootstrap, management"},
		{"bad clusters", "  - manifest: {path: a.yaml}\n    clusters: [kind]\n", `"kind" is neither bootstrap nor management`},
		{"remote chart without version", "  - helm: {chart: oci://ghcr.io/x/charts/proxy, release: proxy, namespace: proxy}\n    clusters: [bootstrap]\n", "version: required for a remote chart"},
		{"bad release", "  - helm: {chart: ./c, release: My_Release, namespace: proxy}\n    clusters: [management]\n", `release: "My_Release"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse("m.yaml", []byte(valid+"beforeApply:\n"+tt.entry))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse: %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadResolvesFilesAndTheirVariables(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("secret.yaml", "stringData:\n  token: ${PROXY_TOKEN}\n  again: ${PROXY_TOKEN}\n")
	write("values.yaml", "password: ${DB_PASSWORD}\n")
	write("cluster.yaml", "")
	write("m.yaml", valid+`beforeApply:
  - manifest: {path: secret.yaml}
    clusters: [bootstrap]
  - helm: {chart: ./chart, release: proxy, namespace: proxy, values: values.yaml}
    clusters: [bootstrap, management]
`)
	c, err := Load(filepath.Join(dir, "m.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Path("secret.yaml"); got != filepath.Join(dir, "secret.yaml") {
		t.Errorf("Path: %q", got)
	}
	if !c.BeforeApply[1].IsOn(OnManagement) || c.BeforeApply[0].IsOn(OnManagement) {
		t.Error("IsOn")
	}

	err = c.CheckEnv(func(k string) (string, bool) {
		if k == "DB_PASSWORD" {
			return "x", true
		}
		return "", false
	})
	var me *MissingEnvError
	if !errors.As(err, &me) || strings.Join(me.Names, ",") != "PROXY_TOKEN" {
		t.Errorf("CheckEnv: %v, want only PROXY_TOKEN missing", err)
	}

	write("m2.yaml", valid+"beforeApply:\n  - manifest: {path: missing.yaml}\n    clusters: [bootstrap]\n")
	if _, err := Load(filepath.Join(dir, "m2.yaml")); err == nil || !strings.Contains(err.Error(), "missing.yaml") {
		t.Errorf("Load with a missing manifest: %v", err)
	}
}
