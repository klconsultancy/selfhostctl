// Package support is the support matrix: what this tool release is built against.
// The README carries the same table.
package support

import (
	"strconv"
	"strings"
)

// CAPIVersion is the Cluster API core this release installs and manages. It matches
// the sigs.k8s.io/cluster-api module in go.mod (docs/design.md#libraries).
const CAPIVersion = "v1.14.4"

// ClusterTopologyGateNeeded reports whether the core still needs CLUSTER_TOPOLOGY=true
// for ClusterClass: before CAPI 1.15 the gate is alpha and off by default.
func ClusterTopologyGateNeeded() bool {
	return minor(CAPIVersion) < 15
}

func minor(v string) int {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(parts[1])
	return n
}
