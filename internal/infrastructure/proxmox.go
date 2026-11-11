package infrastructure

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// proxmoxAPI reads the Proxmox VE API with the credentials CAPMOX uses: GETs only.
type proxmoxAPI struct {
	url  string
	auth string
	http *http.Client
}

// newProxmoxAPI reads the keys of a CAPMOX credentials Secret: url, token, secret,
// root_ca and insecure. Unlike CAPMOX, an unset insecure means the certificate is
// checked: CAPMOX's token, which may destroy VMs, leaves the operator's host here.
func newProxmoxAPI(data map[string][]byte) (*proxmoxAPI, error) {
	var missing []string
	for _, k := range []string{"url", "token", "secret"} {
		if len(data[k]) == 0 {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("empty or unset: %s", strings.Join(missing, ", "))
	}
	base, err := url.JoinPath(string(data["url"]), "api2", "json")
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	insecure, set := data["insecure"]
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Only when the Secret says so, in CAPMOX's true-ish values.
		InsecureSkipVerify: set && slices.Contains([]string{"1", "on", "true", "yes", "y"}, strings.ToLower(string(insecure))), //nolint:gosec // the operator's explicit choice in the Secret
	}
	if ca := data["root_ca"]; len(ca) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("root_ca: no certificate in it")
		}
		tlsConfig.RootCAs = pool
	}
	return &proxmoxAPI{
		url:  base,
		auth: "PVEAPIToken=" + string(data["token"]) + "=" + string(data["secret"]),
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig}},
	}, nil
}

// proxmoxVM is a VM as the cluster's resources list it.
type proxmoxVM struct {
	VMID int64  `json:"vmid"`
	Name string `json:"name"`
	Node string `json:"node"`
	// Status is running or stopped.
	Status string `json:"status"`
}

type proxmoxNode struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

// vms are every VM in the Proxmox cluster, on any node.
func (p *proxmoxAPI) vms(ctx context.Context) ([]proxmoxVM, error) {
	var out []proxmoxVM
	return out, p.get(ctx, "cluster/resources?type=vm", &out)
}

// onlineNodes are the names of the nodes that are online, sorted.
func (p *proxmoxAPI) onlineNodes(ctx context.Context) ([]string, error) {
	var nodes []proxmoxNode
	if err := p.get(ctx, "nodes", &nodes); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nodes {
		if n.Status == "online" {
			out = append(out, n.Node)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (p *proxmoxAPI) get(ctx context.Context, path string, data any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+"/"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", p.auth)
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /%s: %s", strings.SplitN(path, "?", 2)[0], resp.Status)
	}
	return json.Unmarshal(body, &struct {
		Data any `json:"data"`
	}{Data: data})
}
