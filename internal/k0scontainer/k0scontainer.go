// Package k0scontainer runs a k0s cluster in one Docker container through the Docker
// SDK: the Bootstrap cluster, and later the Break-glass controller (docs/adr/0001).
package k0scontainer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Docker is the part of the Docker SDK client this package uses; *client.Client
// implements it.
type Docker interface {
	ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, id string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerRemove(ctx context.Context, id string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	CopyFromContainer(ctx context.Context, id string, options client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	ImageInspect(ctx context.Context, ref string, options ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	NetworkInspect(ctx context.Context, id string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
}

// Mount is a bind mount from the host.
type Mount struct {
	HostPath, ContainerPath string
	ReadOnly                bool
}

// Spec is the container a Cluster runs.
type Spec struct {
	// Name of the container, and its hostname.
	Name string
	// Image is a k0s image, e.g. docker.io/k0sproject/k0s:v1.36.5-k0s.0.
	Image string
	// Args follow `k0s`, e.g. controller --enable-worker --no-taints.
	Args []string
	// Network to join; empty for Docker's default.
	Network string
	Mounts  []Mount
	Labels  map[string]string
}

// Cluster is one k0s container.
type Cluster struct {
	Docker Docker
	Spec   Spec
}

const apiPort = "6443/tcp"

// Observed is what Inspect finds.
type Observed struct {
	Exists, Running bool
	// Image the existing container was created from.
	Image string
}

// Inspect looks the container up by name.
func (c *Cluster) Inspect(ctx context.Context) (Observed, error) {
	res, err := c.Docker.ContainerInspect(ctx, c.Spec.Name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return Observed{}, nil
	}
	if err != nil {
		return Observed{}, fmt.Errorf("inspect container %s: %w", c.Spec.Name, err)
	}
	o := Observed{Exists: true}
	if res.Container.State != nil {
		o.Running = res.Container.State.Running
	}
	if res.Container.Config != nil {
		o.Image = res.Container.Config.Image
	}
	return o, nil
}

// Ensure creates the container when it doesn't exist and starts it when it doesn't
// run. An existing container is otherwise left as it is.
func (c *Cluster) Ensure(ctx context.Context) error {
	o, err := c.Inspect(ctx)
	if err != nil {
		return err
	}
	if !o.Exists {
		if err := c.create(ctx); err != nil {
			return err
		}
	}
	if !o.Running {
		if _, err := c.Docker.ContainerStart(ctx, c.Spec.Name, client.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("start container %s: %w", c.Spec.Name, err)
		}
	}
	return nil
}

func (c *Cluster) create(ctx context.Context) error {
	if err := c.pull(ctx); err != nil {
		return err
	}
	if err := c.ensureNetwork(ctx); err != nil {
		return err
	}
	port := network.MustParsePort(apiPort)
	mounts := make([]mount.Mount, len(c.Spec.Mounts))
	for i, m := range c.Spec.Mounts {
		mounts[i] = mount.Mount{Type: mount.TypeBind, Source: m.HostPath, Target: m.ContainerPath, ReadOnly: m.ReadOnly}
	}
	host := &container.HostConfig{
		// k0s runs containerd, kubelet and its own networking inside.
		Privileged: true,
		Tmpfs:      map[string]string{"/run": ""},
		// The API on localhost only, on a port Docker picks.
		PortBindings: network.PortMap{port: {{HostIP: netip.MustParseAddr("127.0.0.1")}}},
		Mounts:       mounts,
	}
	if c.Spec.Network != "" {
		host.NetworkMode = container.NetworkMode(c.Spec.Network)
	}
	_, err := c.Docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: c.Spec.Name,
		Config: &container.Config{
			Image:        c.Spec.Image,
			Hostname:     c.Spec.Name,
			Cmd:          append([]string{"k0s"}, c.Spec.Args...),
			ExposedPorts: network.PortSet{port: {}},
			// k0s's advice for k0s in Docker: its data dir off the overlay filesystem.
			Volumes: map[string]struct{}{"/var/lib/k0s": {}},
			Labels:  c.Spec.Labels,
		},
		HostConfig: host,
	})
	if err != nil {
		return fmt.Errorf("create container %s: %w", c.Spec.Name, err)
	}
	return nil
}

func (c *Cluster) pull(ctx context.Context) error {
	if _, err := c.Docker.ImageInspect(ctx, c.Spec.Image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %s: %w", c.Spec.Image, err)
	}
	res, err := c.Docker.ImagePull(ctx, c.Spec.Image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", c.Spec.Image, err)
	}
	defer func() { _ = res.Close() }()
	if err := res.Wait(ctx); err != nil {
		return fmt.Errorf("pull %s: %w", c.Spec.Image, err)
	}
	return nil
}

func (c *Cluster) ensureNetwork(ctx context.Context) error {
	if c.Spec.Network == "" {
		return nil
	}
	_, err := c.Docker.NetworkInspect(ctx, c.Spec.Network, client.NetworkInspectOptions{})
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect network %s: %w", c.Spec.Network, err)
	}
	if _, err := c.Docker.NetworkCreate(ctx, c.Spec.Network, client.NetworkCreateOptions{Driver: "bridge"}); err != nil && !cerrdefs.IsConflict(err) {
		return fmt.Errorf("create network %s: %w", c.Spec.Network, err)
	}
	return nil
}

// ErrNotReady: the container runs but k0s hasn't written its admin kubeconfig yet.
var ErrNotReady = errors.New("k0s has not written its admin kubeconfig yet")

// Kubeconfig returns k0s's admin kubeconfig, pointed at the API's published port.
func (c *Cluster) Kubeconfig(ctx context.Context) ([]byte, error) {
	res, err := c.Docker.ContainerInspect(ctx, c.Spec.Name, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", c.Spec.Name, err)
	}
	var hostPort string
	if ns := res.Container.NetworkSettings; ns != nil {
		if b := ns.Ports[network.MustParsePort(apiPort)]; len(b) > 0 {
			hostPort = b[0].HostPort
		}
	}
	if hostPort == "" {
		return nil, fmt.Errorf("container %s publishes no API port", c.Spec.Name)
	}
	conf, err := c.readFile(ctx, "/var/lib/k0s/pki/admin.conf")
	if cerrdefs.IsNotFound(err) || (err == nil && len(conf) == 0) {
		return nil, ErrNotReady
	}
	if err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(conf, []byte("https://localhost:6443"), []byte("https://127.0.0.1:"+hostPort)), nil
}

// readFile reads one file out of the container, through the API's tar stream.
func (c *Cluster) readFile(ctx context.Context, path string) ([]byte, error) {
	res, err := c.Docker.CopyFromContainer(ctx, c.Spec.Name, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Content.Close() }()
	tr := tar.NewReader(res.Content)
	if _, err := tr.Next(); err != nil {
		return nil, fmt.Errorf("read %s from %s: %w", path, c.Spec.Name, err)
	}
	return io.ReadAll(tr)
}

// Remove deletes the container and its data volume.
func (c *Cluster) Remove(ctx context.Context) error {
	_, err := c.Docker.ContainerRemove(ctx, c.Spec.Name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove container %s: %w", c.Spec.Name, err)
	}
	return nil
}
