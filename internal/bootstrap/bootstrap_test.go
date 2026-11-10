package bootstrap

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"io"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/klconsultancy/selfhostctl/internal/beforeapply"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
)

// fakeDocker keeps containers in memory. A started container has its admin.conf at
// once.
type fakeDocker struct {
	containers map[string]*fakeContainer
	created    []client.ContainerCreateOptions
	// memory is Docker's MemTotal; 8 GiB when zero.
	memory int64
	// inotify is what the host's inotify limits read as; kind's values when empty.
	inotify string
	execs   []string
}

type fakeContainer struct {
	image   string
	running bool
	files   map[string][]byte
	modes   map[string]int64
}

// fakePort is the API port Docker published for a container: the Break-glass
// controller's differs from the Bootstrap cluster's, so their kubeconfigs do.
func fakePort(name string) string {
	if strings.HasSuffix(name, "-break-glass") {
		return "32002"
	}
	return "32001"
}

func newFakeDocker() *fakeDocker { return &fakeDocker{containers: map[string]*fakeContainer{}} }

func (d *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	c, ok := d.containers[id]
	if !ok {
		return client.ContainerInspectResult{}, cerrdefs.ErrNotFound
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{
		State:  &container.State{Running: c.running},
		Config: &container.Config{Image: c.image},
		NetworkSettings: &container.NetworkSettings{Ports: network.PortMap{
			network.MustParsePort("6443/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: fakePort(id)}},
		}},
	}}, nil
}

func (d *fakeDocker) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	d.created = append(d.created, o)
	d.containers[o.Name] = &fakeContainer{image: o.Config.Image}
	return client.ContainerCreateResult{ID: o.Name}, nil
}

func (d *fakeDocker) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	d.containers[id].running = true
	return client.ContainerStartResult{}, nil
}

func (d *fakeDocker) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	delete(d.containers, id)
	return client.ContainerRemoveResult{}, nil
}

func (d *fakeDocker) CopyFromContainer(_ context.Context, id string, o client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	if c, ok := d.containers[id]; !ok || !c.running {
		return client.CopyFromContainerResult{}, cerrdefs.ErrNotFound
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte("server: https://localhost:6443\n")
	if o.SourcePath == "/tmp/selfhostctl-output" {
		content = []byte(cmp.Or(d.inotify, "524288\n512\n"))
	}
	_ = tw.WriteHeader(&tar.Header{Name: "admin.conf", Mode: 0o600, Size: int64(len(content))})
	_, _ = tw.Write(content)
	_ = tw.Close()
	return client.CopyFromContainerResult{Content: io.NopCloser(&buf)}, nil
}

func (d *fakeDocker) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	return client.ImageInspectResult{}, nil
}

func (d *fakeDocker) ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
	panic("the image is present")
}

func (d *fakeDocker) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	return client.NetworkInspectResult{}, nil
}

func (d *fakeDocker) NetworkCreate(context.Context, string, client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	panic("the network exists")
}

func (d *fakeDocker) CopyToContainer(_ context.Context, id string, o client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	c := d.containers[id]
	c.files, c.modes = map[string][]byte{}, map[string]int64{}
	tr := tar.NewReader(o.Content)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return client.CopyToContainerResult{}, nil
		}
		if err != nil {
			return client.CopyToContainerResult{}, err
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			c.files["/"+h.Name], c.modes["/"+h.Name] = b, h.Mode
		}
	}
}

func (d *fakeDocker) ExecCreate(_ context.Context, id string, o client.ExecCreateOptions) (client.ExecCreateResult, error) {
	d.execs = append(d.execs, id+": "+strings.Join(o.Cmd, " "))
	return client.ExecCreateResult{ID: "exec"}, nil
}

func (d *fakeDocker) ExecStart(context.Context, string, client.ExecStartOptions) (client.ExecStartResult, error) {
	return client.ExecStartResult{}, nil
}

func (d *fakeDocker) ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error) {
	return client.ExecInspectResult{}, nil
}

func (d *fakeDocker) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	return client.SystemInfoResult{Info: system.Info{MemTotal: cmp.Or(d.memory, 8<<30)}}, nil
}

const testYAML = `
apiVersion: selfhostctl/v1alpha1
kind: ManagementCluster
metadata:
  name: mgmt
providers:
  bootstrap: [{name: k0sproject-k0smotron, version: v2.3.0}]
  controlPlane: [{name: k0sproject-k0smotron, version: v2.3.0}]
  infrastructure: [{name: docker, version: v1.14.4}]
cluster:
  manifests: cluster.yaml
  namespace: mgmt
  kubeconfig: /nonexistent/selfhostctl-test/mgmt.kubeconfig
bootstrapCluster:
  network: kind
`

func testConfig(t *testing.T, more string) *config.ManagementCluster {
	t.Helper()
	c, err := config.Parse("test.yaml", []byte(testYAML+more))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeInstaller keeps one cluster's providers in memory.
type fakeInstaller struct {
	installed []providers.Installed
	inits     []providers.Wanted
}

func (f *fakeInstaller) Installed(context.Context, []byte) ([]providers.Installed, error) {
	return f.installed, nil
}

func (f *fakeInstaller) Init(_ context.Context, _ []byte, w providers.Wanted) error {
	f.inits = append(f.inits, w)
	fp := providers.Fingerprint(w.Variables)
	for _, p := range w.Providers {
		p.URL = ""
		f.installed = append(f.installed, providers.Installed{Provider: p, Fingerprint: fp})
	}
	return nil
}

func deps(d *fakeDocker, kubeconfigs *[]string) Deps {
	return Deps{
		Docker: d,
		NodesReady: func(_ context.Context, kc []byte) (bool, error) {
			*kubeconfigs = append(*kubeconfigs, string(kc))
			return true, nil
		},
		ReadyTimeout: time.Second,
		Providers:    &fakeInstaller{},
		// These tests start from nothing; locate's guard has its own tests.
		First: true,
	}
}

func TestProvidersPhase(t *testing.T) {
	d := newFakeDocker()
	var kubeconfigs []string
	dp := deps(d, &kubeconfigs)
	inst := dp.Providers.(*fakeInstaller)
	cfg := testConfig(t, "")

	st, err := Phases(cfg, dp)[1].Status(t.Context())
	if err != nil || st.State != phase.Blocked || !strings.Contains(st.Reason, "Bootstrap cluster doesn't run") {
		t.Fatalf("providers before the Bootstrap cluster: %v %v, want blocked on it", st, err)
	}

	if err := phase.Run(t.Context(), Phases(cfg, dp)[:2], "", io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(inst.inits) != 1 {
		t.Fatalf("clusterctl init ran %d times, want once", len(inst.inits))
	}
	if err := phase.Run(t.Context(), Phases(cfg, dp)[:2], "", io.Discard); err != nil || len(inst.inits) != 1 {
		t.Errorf("second run: %v, init ran %d times", err, len(inst.inits))
	}

	inst.installed[1].Version = "v2.2.0"
	st, err = Phases(cfg, dp)[1].Status(t.Context())
	if err != nil || st.State != phase.Blocked || !strings.Contains(st.Reason, "bootstrap-k0sproject-k0smotron is at v2.2.0") {
		t.Errorf("mismatched install: %v %v, want blocked with the difference", st, err)
	}
}

func TestBootstrapClusterPhase(t *testing.T) {
	d := newFakeDocker()
	var kubeconfigs []string
	p := Phases(testConfig(t, ""), deps(d, &kubeconfigs))[0]
	if st, err := p.Status(t.Context()); err != nil || st.State != phase.Next {
		t.Fatalf("before: %v, %v; want next", st, err)
	}
	if err := phase.Run(t.Context(), Phases(testConfig(t, ""), deps(d, &kubeconfigs))[:1], "", io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(d.created) != 1 {
		t.Fatalf("created %d containers, want 1", len(d.created))
	}
	o := d.created[0]
	if o.Name != "selfhostctl-mgmt-bootstrap" || o.Config.Image != Image || !o.HostConfig.Privileged {
		t.Errorf("container: name %q image %q privileged %v", o.Name, o.Config.Image, o.HostConfig.Privileged)
	}
	if string(o.HostConfig.NetworkMode) != "kind" || o.Config.Labels[LabelRole] != "bootstrap" {
		t.Errorf("network %q labels %v", o.HostConfig.NetworkMode, o.Config.Labels)
	}
	// Nothing from the host unless the config declares it: the Docker socket gives
	// control over the host.
	if len(o.HostConfig.Mounts) != 0 || len(o.HostConfig.Binds) != 0 {
		t.Errorf("host mounts without any declared: %v %v", o.HostConfig.Mounts, o.HostConfig.Binds)
	}
	for _, b := range o.HostConfig.PortBindings[network.MustParsePort("6443/tcp")] {
		if b.HostIP != netip.MustParseAddr("127.0.0.1") {
			t.Errorf("API published on %v, want localhost only", b.HostIP)
		}
	}
	if want := "server: https://127.0.0.1:32001\n"; kubeconfigs[len(kubeconfigs)-1] != want {
		t.Errorf("kubeconfig %q, want %q", kubeconfigs[len(kubeconfigs)-1], want)
	}

	// A second run changes nothing.
	if err := phase.Run(t.Context(), Phases(testConfig(t, ""), deps(d, &kubeconfigs))[:1], "", io.Discard); err != nil || len(d.created) != 1 {
		t.Errorf("second run: %v, created %d", err, len(d.created))
	}
}

func TestBootstrapClusterPhaseMountsOnlyWhatIsDeclared(t *testing.T) {
	d := newFakeDocker()
	var kubeconfigs []string
	cfg := testConfig(t, "  extraMounts:\n    - hostPath: /var/run/docker.sock\n      containerPath: /var/run/docker.sock\n")
	if err := phase.Run(t.Context(), Phases(cfg, deps(d, &kubeconfigs))[:1], "", io.Discard); err != nil {
		t.Fatal(err)
	}
	m := d.created[0].HostConfig.Mounts
	if len(m) != 1 || m[0].Source != "/var/run/docker.sock" || m[0].Target != "/var/run/docker.sock" {
		t.Errorf("mounts %v, want only the declared Docker socket", m)
	}
}

func TestBootstrapClusterPhaseStartsAStoppedContainer(t *testing.T) {
	d := newFakeDocker()
	d.containers["selfhostctl-mgmt-bootstrap"] = &fakeContainer{image: Image}
	var kubeconfigs []string
	if err := phase.Run(t.Context(), Phases(testConfig(t, ""), deps(d, &kubeconfigs))[:1], "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(d.created) != 0 || !d.containers["selfhostctl-mgmt-bootstrap"].running {
		t.Errorf("created %d, running %v; want the existing container started", len(d.created), d.containers["selfhostctl-mgmt-bootstrap"].running)
	}
}

func TestBootstrapClusterPhaseBlocksOnAnotherImage(t *testing.T) {
	d := newFakeDocker()
	d.containers["selfhostctl-mgmt-bootstrap"] = &fakeContainer{image: "docker.io/k0sproject/k0s:v1.35.0-k0s.0", running: true}
	var kubeconfigs []string
	st, err := Phases(testConfig(t, ""), deps(d, &kubeconfigs))[0].Status(t.Context())
	if err != nil || st.State != phase.Blocked {
		t.Fatalf("Status: %v, %v; want blocked", st, err)
	}
}

// fakePrereqs keeps the state of each prerequisite by name.
type fakePrereqs struct {
	state   map[string]beforeapply.State
	applied []string
}

func (f *fakePrereqs) State(_ context.Context, _ []byte, p config.Prerequisite) (beforeapply.State, error) {
	return f.state[p.Name()], nil
}

func (f *fakePrereqs) Apply(_ context.Context, _ []byte, p config.Prerequisite) error {
	f.applied = append(f.applied, p.Name())
	f.state[p.Name()] = beforeapply.State{Done: true}
	return nil
}

func TestBeforeApplyPhase(t *testing.T) {
	d := newFakeDocker()
	var kubeconfigs []string
	dp := deps(d, &kubeconfigs)
	pre := &fakePrereqs{state: map[string]beforeapply.State{"manifest b.yaml": {Done: true}}}
	dp.Prerequisites = pre
	cfg := testConfig(t, `beforeApply:
  - manifest: {path: a.yaml}
    clusters: [bootstrap]
  - manifest: {path: b.yaml}
    clusters: [bootstrap, management]
  - manifest: {path: only-target.yaml}
    clusters: [management]
`)
	if err := phase.Run(t.Context(), Phases(cfg, dp)[:3], "", io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// b was done already; only-target is not for the Bootstrap cluster.
	if want := []string{"manifest a.yaml"}; !slices.Equal(pre.applied, want) {
		t.Errorf("applied %v, want %v", pre.applied, want)
	}
	if st, _ := Phases(cfg, dp)[2].Status(t.Context()); st.State != phase.Done {
		t.Errorf("after: %v, want done", st)
	}

	pre.state["manifest a.yaml"] = beforeapply.State{Blocked: "a pending Helm release"}
	if st, _ := Phases(cfg, dp)[2].Status(t.Context()); st.State != phase.Blocked || !strings.Contains(st.Reason, "manifest a.yaml: a pending Helm release") {
		t.Errorf("blocked entry: %v", st)
	}
}
