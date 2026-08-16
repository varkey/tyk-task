// Package k8shealth answers two of the assignment's questions about the
// cluster: whether the API server can be reached (story 3), and whether
// every Deployment has as many ready pods as it asked for (story 1).
package k8shealth

import (
	"k8s.io/client-go/kubernetes"
)

// GetKubernetesVersion returns a string GitVersion of the Kubernetes server defined by the clientset.
//
// If it can't connect an error will be returned, which makes it useful to check connectivity.
func GetKubernetesVersion(clientset kubernetes.Interface) (string, error) {
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}

	return version.String(), nil
}
