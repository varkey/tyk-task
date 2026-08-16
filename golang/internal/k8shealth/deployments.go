package k8shealth

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/varkey/tyk-task/golang/internal/apierr"
)

// DeploymentStatus reports the observed replica health of a single Deployment.
type DeploymentStatus struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Desired   int32  `json:"desiredReplicas"`
	Ready     int32  `json:"readyReplicas"`
	Healthy   bool   `json:"healthy"`
}

// DeploymentHealthReport summarizes the replica health of every Deployment
// considered by CheckDeploymentHealth.
type DeploymentHealthReport struct {
	AllHealthy  bool               `json:"allHealthy"`
	Deployments []DeploymentStatus `json:"deployments"`
}

// CheckDeploymentHealth lists Deployments in namespace (pass "" to consider
// every namespace in the cluster) and reports, for each, whether the number
// of ready pods matches the number requested by its spec - i.e. whether it
// has as many healthy pods as the Deployment asks for.
//
// A Deployment with a nil spec.Replicas defaults to 1, matching the
// Kubernetes API server's own defaulting behaviour.
func CheckDeploymentHealth(ctx context.Context, clientset kubernetes.Interface, namespace string) (DeploymentHealthReport, error) {
	list, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			// In namespace-scoped RBAC mode, listing without a namespace
			// (or naming one outside the configured set) is flatly denied
			// by the apiserver - it doesn't get narrowed to whatever this
			// service's Roles happen to cover. See
			// CheckDeploymentHealthAcrossNamespaces for the fix.
			if namespace == "" {
				return DeploymentHealthReport{}, &apierr.PermissionError{Msg: "not permitted to list deployments cluster-wide"}
			}
			return DeploymentHealthReport{}, &apierr.PermissionError{Msg: fmt.Sprintf("not permitted to list deployments in namespace %q", namespace)}
		}
		return DeploymentHealthReport{}, fmt.Errorf("listing deployments: %w", err)
	}

	report := DeploymentHealthReport{
		AllHealthy:  true,
		Deployments: make([]DeploymentStatus, 0, len(list.Items)),
	}

	for _, d := range list.Items {
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}

		healthy := d.Status.ReadyReplicas == desired
		if !healthy {
			report.AllHealthy = false
		}

		report.Deployments = append(report.Deployments, DeploymentStatus{
			Name:      d.Name,
			Namespace: d.Namespace,
			Desired:   desired,
			Ready:     d.Status.ReadyReplicas,
			Healthy:   healthy,
		})
	}

	return report, nil
}

// CheckDeploymentHealthAcrossNamespaces merges CheckDeploymentHealth across
// each of namespaces (one List call per namespace) rather than a single
// cluster-wide call.
//
// This is what a namespace-scoped RBAC deployment must use instead of
// CheckDeploymentHealth(ctx, clientset, ""): a Role/RoleBinding never
// authorizes a cluster-wide List, no matter how many namespaces it covers -
// the apiserver rejects that request outright rather than narrowing it (see
// CheckDeploymentHealth's IsForbidden handling). Iterating per namespace is
// the only way to get the equivalent result under that RBAC shape.
func CheckDeploymentHealthAcrossNamespaces(ctx context.Context, clientset kubernetes.Interface, namespaces []string) (DeploymentHealthReport, error) {
	merged := DeploymentHealthReport{AllHealthy: true, Deployments: []DeploymentStatus{}}

	for _, ns := range namespaces {
		r, err := CheckDeploymentHealth(ctx, clientset, ns)
		if err != nil {
			return DeploymentHealthReport{}, err
		}
		merged.Deployments = append(merged.Deployments, r.Deployments...)
		if !r.AllHealthy {
			merged.AllHealthy = false
		}
	}

	return merged, nil
}
