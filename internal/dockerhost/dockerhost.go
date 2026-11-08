// Package dockerhost finds the Docker daemon the operator's Docker CLI would use, and
// connects the Docker SDK to it.
//
// The SDK reads DOCKER_HOST but not the CLI's contexts, while colima, Docker Desktop
// and Rancher Desktop reach their daemon through a context. The CLI's config and
// context metadata are read as files; the `docker` binary is never run.
package dockerhost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/moby/moby/client"
)

// Env is what Endpoint reads from the environment; os.LookupEnv and os.UserHomeDir
// outside tests.
type Env struct {
	Lookup func(string) (string, bool)
	Home   func() (string, error)
}

// OS is the real environment.
var OS = Env{Lookup: os.LookupEnv, Home: os.UserHomeDir}

// Endpoint returns the daemon's address: DOCKER_HOST when set, else the endpoint of
// the active context (DOCKER_CONTEXT, else the CLI config's currentContext). An empty
// result means the SDK's default, as for the CLI's "default" context.
func Endpoint(env Env) (string, error) {
	if h, ok := env.Lookup("DOCKER_HOST"); ok && h != "" {
		return h, nil
	}
	dir, err := configDir(env)
	if err != nil {
		return "", err
	}
	name, _ := env.Lookup("DOCKER_CONTEXT")
	if name == "" {
		name, err = currentContext(dir)
		if err != nil {
			return "", err
		}
	}
	if name == "" || name == "default" {
		return "", nil
	}
	return contextHost(dir, name)
}

// New returns an SDK client for the daemon Endpoint finds.
func New(env Env) (*client.Client, error) {
	host, err := Endpoint(env)
	if err != nil {
		return nil, err
	}
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	return client.New(opts...)
}

func configDir(env Env) (string, error) {
	if d, ok := env.Lookup("DOCKER_CONFIG"); ok && d != "" {
		return d, nil
	}
	home, err := env.Home()
	if err != nil {
		return "", fmt.Errorf("docker config: %w", err)
	}
	return filepath.Join(home, ".docker"), nil
}

func currentContext(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("docker config: %w", err)
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("docker config %s: %w", filepath.Join(dir, "config.json"), err)
	}
	return cfg.CurrentContext, nil
}

// contextHost reads a context's metadata, which the CLI keeps under
// contexts/meta/<sha256 of the name>/meta.json.
func contextHost(dir, name string) (string, error) {
	sum := sha256.Sum256([]byte(name))
	file := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("docker context %q: %w", name, err)
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("docker context %q: %w", name, err)
	}
	host := meta.Endpoints["docker"].Host
	if host == "" {
		return "", fmt.Errorf("docker context %q has no docker endpoint", name)
	}
	return host, nil
}
