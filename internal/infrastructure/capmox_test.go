package infrastructure

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// fakeProxmox serves the cluster's VMs and nodes, for the token the Secret holds.
type fakeProxmox struct {
	vms   []proxmoxVM
	nodes []proxmoxNode
}

func (f *fakeProxmox) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=capmox@pve!capi=s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var data any
		switch {
		case r.URL.Path == "/api2/json/cluster/resources" && r.URL.Query().Get("type") == "vm":
			data = f.vms
		case r.URL.Path == "/api2/json/nodes":
			data = f.nodes
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func object(gvk schema.GroupVersionKind, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: fields}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace("mgmt")
	u.SetName(name)
	return u
}

// capmoxWorld is a Cluster on a ProxmoxCluster with one old Machine, VM 101.
type capmoxWorld struct {
	proxmox  *fakeProxmox
	srv      *httptest.Server
	url      string
	zones    bool
	fds      bool
	noCreds  bool
	node     string
	noVMID   bool
	secret   map[string][]byte
	machines Machines
}

func newCapmoxWorld(t *testing.T) *capmoxWorld {
	w := &capmoxWorld{proxmox: &fakeProxmox{
		nodes: []proxmoxNode{{Node: "pve1", Status: "offline"}, {Node: "pve2", Status: "online"}, {Node: "pve3", Status: "online"}},
	}}
	w.srv = w.proxmox.serve(t)
	w.url = w.srv.URL
	w.secret = map[string][]byte{"url": []byte(w.url), "token": []byte("capmox@pve!capi"), "secret": []byte("s3cret")}
	return w
}

func (w *capmoxWorld) build() Machines {
	cluster := object(clusterGVK, "mgmt", map[string]any{"spec": map[string]any{
		"infrastructureRef": map[string]any{"apiGroup": proxmoxClusterGVK.Group, "kind": "ProxmoxCluster", "name": "mgmt"}}})
	if w.fds {
		cluster.Object["status"] = map[string]any{"failureDomains": []any{map[string]any{"name": "zone-a", "controlPlane": true}}}
	}
	pcSpec := map[string]any{}
	if !w.noCreds {
		pcSpec["credentialsRef"] = map[string]any{"name": "proxmox-creds"}
	}
	if w.zones {
		pcSpec["zoneConfig"] = []any{map[string]any{"zone": "zone-a"}}
	}
	pc := object(proxmoxClusterGVK, "mgmt", map[string]any{"spec": pcSpec})
	machine := object(machineGVK, "mgmt-old-1", map[string]any{"spec": map[string]any{
		"infrastructureRef": map[string]any{"apiGroup": proxmoxMachineGVK.Group, "kind": "ProxmoxMachine", "name": "mgmt-old-1"}}})
	pmSpec := map[string]any{}
	if !w.noVMID {
		pmSpec["virtualMachineID"] = int64(101)
	}
	pm := object(proxmoxMachineGVK, "mgmt-old-1", map[string]any{"spec": pmSpec})
	if w.node != "" {
		pm.Object["status"] = map[string]any{"proxmoxNode": w.node}
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "mgmt", Name: "proxmox-creds"}, Data: w.secret}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{clusterGVK, machineGVK, proxmoxClusterGVK, proxmoxMachineGVK} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(cluster, pc, machine, pm, secret).WithStatusSubresource(pm, cluster).Build()
	w.machines = Machines{Client: c, Namespace: "mgmt", Cluster: "mgmt", Names: []string{"mgmt-old-1", "mgmt-gone"}}
	return w.machines
}

func (w *capmoxWorld) node101(t *testing.T) string {
	t.Helper()
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	if err := w.machines.Client.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "mgmt", Name: "mgmt-old-1"}, pm); err != nil {
		t.Fatal(err)
	}
	n, _, _ := unstructured.NestedString(pm.Object, "status", "proxmoxNode")
	return n
}

var adapter = capmox{timeout: 50 * time.Millisecond, poll: 5 * time.Millisecond}

func TestCapmoxGone(t *testing.T) {
	for _, tt := range []struct {
		name   string
		setup  func(*capmoxWorld)
		want   phase.State
		reason string
	}{
		{"the VM is gone", func(*capmoxWorld) {}, phase.Done, "VMs are gone from Proxmox (1)"},
		{"the VM still exists", func(w *capmoxWorld) {
			w.proxmox.vms = []proxmoxVM{{VMID: 101, Name: "mgmt-old-1", Node: "pve1", Status: "stopped"}}
		}, phase.Blocked, "VM 101 (mgmt-old-1) on pve1, stopped. Start them"},
		{"another VM took its VMID", func(w *capmoxWorld) {
			w.proxmox.vms = []proxmoxVM{{VMID: 101, Name: "other", Node: "pve2", Status: "running"}}
		}, phase.Blocked, `VMID 101 of ProxmoxMachine mgmt-old-1 is now VM "other" on pve2`},
		{"no VMID", func(w *capmoxWorld) {
			w.noVMID = true
			w.proxmox.vms = []proxmoxVM{{VMID: 101, Name: "mgmt-old-1"}}
		}, phase.Done, "no old ProxmoxMachine has a VM"},
		{"no credentialsRef", func(w *capmoxWorld) { w.noCreds = true }, phase.Blocked, "has no spec.credentialsRef"},
		{"a credential missing", func(w *capmoxWorld) { delete(w.secret, "token") }, phase.Blocked, "empty or unset: token"},
		{"the wrong token", func(w *capmoxWorld) { w.secret["secret"] = []byte("wrong") }, phase.Blocked, "doesn't answer: GET /cluster/resources: 401"},
		{"certificates checked", func(w *capmoxWorld) { w.secret["insecure"] = []byte("false") }, phase.Blocked, "certificate"},
		{"a root CA", func(w *capmoxWorld) {
			w.secret["insecure"] = []byte("false")
			w.secret["root_ca"] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: w.srv.Certificate().Raw})
		}, phase.Done, "VMs are gone from Proxmox (1)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newCapmoxWorld(t)
			tt.setup(w)
			st, err := adapter.Gone(t.Context(), w.build())
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tt.want || !strings.Contains(st.Reason, tt.reason) {
				t.Errorf("got %s %q", st.State, st.Reason)
			}
		})
	}
}

// An old ProxmoxMachine whose node is offline or unknown is pointed at an online one.
func TestCapmoxPointsAtAnOnlineNode(t *testing.T) {
	for _, node := range []string{"", "pve1"} {
		w := newCapmoxWorld(t)
		w.node = node
		m := w.build()
		st, _ := adapter.Status(t.Context(), m)
		if st.State != phase.Next || !strings.Contains(st.Reason, "node pve2") {
			t.Fatalf("node %q: before %s %q", node, st.State, st.Reason)
		}
		if err := adapter.Run(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if got := w.node101(t); got != "pve2" {
			t.Errorf("node %q: proxmoxNode %q", node, got)
		}
		if st, _ := adapter.Status(t.Context(), m); st.State != phase.Done {
			t.Errorf("node %q: after %s %q", node, st.State, st.Reason)
		}
	}
}

func TestCapmoxKeepsAnOnlineNode(t *testing.T) {
	w := newCapmoxWorld(t)
	w.node = "pve3"
	m := w.build()
	if st, _ := adapter.Status(t.Context(), m); st.State != phase.Done {
		t.Errorf("got %s %q", st.State, st.Reason)
	}
	if err := adapter.Run(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if got := w.node101(t); got != "pve3" {
		t.Errorf("proxmoxNode %q", got)
	}
}

func TestCapmoxStatusBlocks(t *testing.T) {
	for _, tt := range []struct {
		name   string
		setup  func(*capmoxWorld)
		reason string
	}{
		{"no node online", func(w *capmoxWorld) { w.proxmox.nodes = []proxmoxNode{{Node: "pve1", Status: "offline"}} }, "no Proxmox node is online"},
		{"the VM came back", func(w *capmoxWorld) {
			w.proxmox.vms = []proxmoxVM{{VMID: 101, Name: "mgmt-old-1", Node: "pve1", Status: "running"}}
		}, "VMs still exist"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newCapmoxWorld(t)
			tt.setup(w)
			m := w.build()
			st, err := adapter.Status(t.Context(), m)
			if err != nil {
				t.Fatal(err)
			}
			if st.State != phase.Blocked || !strings.Contains(st.Reason, tt.reason) {
				t.Errorf("got %s %q", st.State, st.Reason)
			}
			if err := adapter.Run(t.Context(), m); err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("run: %v", err)
			}
		})
	}
}

// With zones, the phase waits for the Cluster's failure domains.
func TestCapmoxWaitsForFailureDomains(t *testing.T) {
	w := newCapmoxWorld(t)
	w.zones, w.node = true, "pve2"
	m := w.build()
	if st, _ := adapter.Status(t.Context(), m); st.State != phase.Next || !strings.Contains(st.Reason, "failure domains") {
		t.Errorf("got %s %q", st.State, st.Reason)
	}
	if err := adapter.Run(t.Context(), m); err == nil || !strings.Contains(err.Error(), "no failure domains") {
		t.Errorf("run: %v", err)
	}

	w.fds = true
	m = w.build()
	if st, _ := adapter.Status(t.Context(), m); st.State != phase.Done {
		t.Errorf("with failure domains: %s %q", st.State, st.Reason)
	}
	if err := adapter.Run(t.Context(), m); err != nil {
		t.Errorf("with failure domains: %v", err)
	}
}

func TestFor(t *testing.T) {
	if _, ok := For(nil).(none); !ok {
		t.Error("the default isn't none")
	}
	if _, ok := For(&config.Infrastructure{Adapter: "capmox"}).(capmox); !ok {
		t.Error("capmox isn't capmox")
	}
}
