package apply

import (
	"errors"
	"testing"
)

func TestWebhookUnreachable(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		// The e2e's: k0smotron's Deployment Available, its webhook Service not yet.
		{`Internal error occurred: failed calling webhook "mutate-k0scontrolplane-v1beta2.k0smotron.io": failed to call webhook: Post "https://k0smotron-webhook-service-control-plane.k0smotron.svc:443/mutate?timeout=10s": dial tcp 10.97.5.27:443: i/o timeout`, true},
		{`failed calling webhook "x": failed to call webhook: Post "https://x": dial tcp 10.0.0.1:443: connect: connection refused`, true},
		{`failed calling webhook "x": failed to call webhook: Post "https://x": no endpoints available for service "x"`, true},
		{`failed calling webhook "x": failed to call webhook: Post "https://x": tls: failed to verify certificate: x509: certificate signed by unknown authority`, true},
		// The webhook answered: it rejects the object. That is the operator's to fix.
		{`admission webhook "validation.k0smotron.io" denied the request: spec.replicas: Invalid value`, false},
		{`K0sControlPlane.controlplane.cluster.x-k8s.io "mgmt" is invalid: spec.version: Required value`, false},
	}
	for _, tt := range tests {
		if got := webhookUnreachable(errors.New(tt.msg)); got != tt.want {
			t.Errorf("webhookUnreachable(%q) = %v, want %v", tt.msg, got, tt.want)
		}
	}
}
