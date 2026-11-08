// Package kube holds small, shared checks against a cluster's API.
package kube

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Client returns a controller-runtime client for a kubeconfig, with the core types and
// those of the given scheme builders.
func Client(kubeconfig []byte, add ...func(*runtime.Scheme) error) (ctrlclient.Client, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	for _, f := range append([]func(*runtime.Scheme) error{clientgoscheme.AddToScheme}, add...) {
		if err := f(scheme); err != nil {
			return nil, err
		}
	}
	return ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
}

// NodesReady reports whether the cluster has at least one Node and all of them are
// Ready.
func NodesReady(ctx context.Context, c ctrlclient.Client) (bool, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return false, err
	}
	if len(nodes.Items) == 0 {
		return false, nil
	}
	for _, n := range nodes.Items {
		ready := false
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			return false, nil
		}
	}
	return true, nil
}
