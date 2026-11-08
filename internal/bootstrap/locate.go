package bootstrap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/apply"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/k0scontainer"
)

// Location is where the Management cluster's objects live (docs/design.md#lifecycle-state).
type Location int

const (
	// LocationUnknown: no handle proves there is no Management cluster yet.
	LocationUnknown Location = iota
	LocationNone
	LocationBootstrap
	// LocationSplit: both clusters hold the Cluster, as a move that failed halfway
	// leaves it.
	LocationSplit
	LocationManagement
)

func (l Location) String() string {
	return [...]string{"unknown", "none", "bootstrap", "split", "management"}[l]
}

// Situation is what locate found.
type Situation struct {
	Location Location
	// Why, for unknown.
	Reason string
	// BootstrapKC is the Bootstrap cluster's kubeconfig while it runs and is ready.
	BootstrapKC []byte
	// BootstrapExists: its container exists, ready or not.
	BootstrapExists bool
	// ManagementKC is the kubeconfig file's content while it reaches the cluster.
	ManagementKC []byte
	// SourcePaused, TargetPaused: the Cluster's spec.paused on each side.
	SourcePaused, TargetPaused bool
	// Unanswered: the kubeconfig file exists but its cluster doesn't answer. Not even
	// --first overrides that.
	Unanswered bool
}

// locator computes the Situation once and keeps it until a phase has run.
type locator struct {
	cfg     *config.ManagementCluster
	cluster *k0scontainer.Cluster
	clients Clients
	// probe reports whether something answers TLS at host:port.
	probe func(ctx context.Context, hostport string) bool

	mu  sync.Mutex
	sit *Situation
}

func (l *locator) invalidate() {
	l.mu.Lock()
	l.sit = nil
	l.mu.Unlock()
}

func (l *locator) get(ctx context.Context) (Situation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sit != nil {
		return *l.sit, nil
	}
	s, err := l.locate(ctx)
	if err != nil {
		return Situation{}, err
	}
	l.sit = &s
	return s, nil
}

// hold reports whether the cluster behind c holds the Management cluster's Cluster
// object, and whether it is paused.
func (l *locator) hold(ctx context.Context, kc []byte) (found, paused bool, err error) {
	c, err := l.clients(kc)
	if err != nil {
		return false, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(clusterGVK)
	err = c.Get(ctx, ctrlclient.ObjectKey{Namespace: l.cfg.Cluster.Namespace, Name: l.cfg.ClusterName()}, u)
	if apierrors.IsNotFound(err) || isNoMatch(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	paused, _, _ = unstructured.NestedBool(u.Object, "spec", "paused")
	return true, paused, nil
}

func (l *locator) locate(ctx context.Context) (Situation, error) {
	var s Situation
	o, err := l.cluster.Inspect(ctx)
	if err != nil {
		return s, err
	}
	s.BootstrapExists = o.Exists
	var inBootstrap bool
	if o.Running {
		kc, err := l.cluster.Kubeconfig(ctx)
		switch {
		case errors.Is(err, k0scontainer.ErrNotReady):
		case err != nil:
			return s, err
		default:
			s.BootstrapKC = kc
			if inBootstrap, s.SourcePaused, err = l.hold(ctx, kc); err != nil {
				// A Bootstrap cluster still starting; it holds nothing yet.
				inBootstrap = false
			}
		}
	}

	path, err := l.cfg.KubeconfigPath()
	if err != nil {
		return s, err
	}
	var inManagement, reachable bool
	kc, err := os.ReadFile(path)
	haveFile := err == nil
	if haveFile {
		var herr error
		inManagement, s.TargetPaused, herr = l.hold(ctx, kc)
		reachable = herr == nil
		if reachable {
			s.ManagementKC = kc
		}
	}

	switch {
	case inBootstrap && inManagement:
		s.Location = LocationSplit
	case inManagement:
		s.Location = LocationManagement
	case inBootstrap:
		s.Location = LocationBootstrap
	case haveFile && !reachable:
		// Before an empty Bootstrap cluster counts as none: after the pivot, a
		// Management cluster that doesn't answer for a moment must not make the
		// Bootstrap cluster, whose providers still run, get the cluster applied again.
		s.Location = LocationUnknown
		s.Unanswered = true
		s.Reason = fmt.Sprintf("the Management cluster in %s doesn't answer: if this is a network blip, run again; if its machines are gone, that's `selfhostctl recover`; if this is a new cluster, remove that stale kubeconfig", path)
	case s.BootstrapExists:
		// This bootstrap's own Bootstrap cluster, made once the run had proved the
		// location none (or the operator asserted it with --first).
		s.Location = LocationNone
	case haveFile:
		s.Location = LocationNone
	default:
		s.Location = LocationUnknown
		s.Reason = fmt.Sprintf("no kubeconfig at %s, so nothing proves there is no Management cluster yet: copy it there, or run with --first if this is the cluster's first bootstrap", path)
	}
	return s, nil
}

// knownEndpoint is the control-plane endpoint as far as the tool knows it without a
// cluster: the kubeconfig file's server, else a controlPlaneEndpoint in the manifests.
func (l *locator) knownEndpoint() string {
	if path, err := l.cfg.KubeconfigPath(); err == nil {
		if cfg, err := clientcmd.LoadFromFile(path); err == nil {
			for _, c := range cfg.Clusters {
				if u, err := url.Parse(c.Server); err == nil && u.Host != "" {
					return u.Host
				}
			}
		}
	}
	objs, err := apply.Load(l.cfg.Path(l.cfg.Cluster.Manifests))
	if err != nil {
		return ""
	}
	for _, o := range objs {
		host, _, _ := unstructured.NestedString(o.Object, "spec", "controlPlaneEndpoint", "host")
		raw, _, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "controlPlaneEndpoint", "port")
		if port := asPort(raw); host != "" && port != 0 {
			return net.JoinHostPort(host, strconv.FormatInt(port, 10))
		}
	}
	return ""
}

// asPort reads a port however the manifest's decoder typed the number: YAML numbers
// arrive as float64 through JSON.
func asPort(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// ProbeTLS reports whether anything completes a TLS handshake at hostport: any answer
// counts, whatever its certificate.
func ProbeTLS(ctx context.Context, hostport string) bool {
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // only whether something answers
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
