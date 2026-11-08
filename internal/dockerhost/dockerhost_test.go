package dockerhost

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// env returns an Env with the given variables and a home directory holding a Docker
// config with one context, "colima", besides the default.
func env(t *testing.T, vars map[string]string, current string) Env {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".docker")
	sum := sha256.Sum256([]byte("colima"))
	meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(meta, "meta.json"), `{"Name":"colima","Endpoints":{"docker":{"Host":"unix:///home/u/.colima/default/docker.sock","SkipTLSVerify":false}}}`)
	if current != "" {
		write(t, filepath.Join(dir, "config.json"), `{"auths":{},"currentContext":"`+current+`"}`)
	}
	return Env{
		Lookup: func(k string) (string, bool) { v, ok := vars[k]; return v, ok },
		Home:   func() (string, error) { return home, nil },
	}
}

func write(t *testing.T, file, content string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEndpoint(t *testing.T) {
	const colima = "unix:///home/u/.colima/default/docker.sock"
	tests := []struct {
		name    string
		vars    map[string]string
		current string
		want    string
	}{
		{"DOCKER_HOST wins", map[string]string{"DOCKER_HOST": "tcp://h:2375", "DOCKER_CONTEXT": "colima"}, "colima", "tcp://h:2375"},
		{"current context", nil, "colima", colima},
		{"DOCKER_CONTEXT over the config", map[string]string{"DOCKER_CONTEXT": "colima"}, "default", colima},
		{"default context", nil, "default", ""},
		{"no config at all", nil, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Endpoint(env(t, tt.vars, tt.current))
			if err != nil || got != tt.want {
				t.Errorf("Endpoint: %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestEndpointUnknownContext(t *testing.T) {
	if _, err := Endpoint(env(t, map[string]string{"DOCKER_CONTEXT": "nope"}, "")); err == nil {
		t.Error("an unknown context must be an error, not the default daemon")
	}
}
