package provision

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// DeleteKubernetesNode removes one node's Kubernetes Node object. A node whose
// VM is gone never reports Ready again, so leaving its Node object behind
// leaves the cluster permanently NotReady and keeps the scheduler counting a
// machine that no longer exists (#314). An already-absent object is success.
func DeleteKubernetesNode(ctx context.Context, kubeconfig []byte, nodeName string) error {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("parse kubeconfig for node deletion: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes client for node deletion: %w", err)
	}
	return deleteKubernetesNode(ctx, clientset, nodeName)
}

func deleteKubernetesNode(ctx context.Context, client kubernetes.Interface, nodeName string) error {
	err := client.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Kubernetes node %q: %w", nodeName, err)
	}
	return nil
}
