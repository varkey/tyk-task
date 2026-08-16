package k8shealth

import (
	"context"
	"fmt"
	"sort"

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
//
// TotalDeployments/HealthyCount/UnhealthyCount are always over the full set
// considered, even if a caller asks the HTTP handler to filter Deployments
// down to only the unhealthy ones - so "3 of 40 unhealthy" stays visible
// however the list itself is trimmed. Deployments is sorted unhealthy-first
// (see finalizeReport) so the entries worth looking at surface without any
// client-side filtering at all.
type DeploymentHealthReport struct {
	AllHealthy       bool               `json:"allHealthy"`
	TotalDeployments int                `json:"totalDeployments"`
	HealthyCount     int                `json:"healthyCount"`
	UnhealthyCount   int                `json:"unhealthyCount"`
	Deployments      []DeploymentStatus `json:"deployments"`
}

// finalizeReport computes the summary counts and sorts deployments
// unhealthy-first (ties broken by namespace, then name, for deterministic
// output) so a plain `curl | jq` shows what's broken at the top without any
// filtering. It's the single place both CheckDeploymentHealth and
// CheckDeploymentHealthAcrossNamespaces build their report from, so the
// counting/sorting logic can't drift between the two call paths.
func finalizeReport(deployments []DeploymentStatus) DeploymentHealthReport {
	sort.SliceStable(deployments, func(i, j int) bool {
		if deployments[i].Healthy != deployments[j].Healthy {
			return !deployments[i].Healthy // unhealthy (false) sorts first
		}
		if deployments[i].Namespace != deployments[j].Namespace {
			return deployments[i].Namespace < deployments[j].Namespace
		}
		return deployments[i].Name < deployments[j].Name
	})

	report := DeploymentHealthReport{
		AllHealthy:       true,
		TotalDeployments: len(deployments),
		Deployments:      deployments,
	}
	for _, d := range deployments {
		if d.Healthy {
			report.HealthyCount++
		} else {
			report.UnhealthyCount++
			report.AllHealthy = false
		}
	}
	return report
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

	deployments := make([]DeploymentStatus, 0, len(list.Items))
	for _, d := range list.Items {
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}

		deployments = append(deployments, DeploymentStatus{
			Name:      d.Name,
			Namespace: d.Namespace,
			Desired:   desired,
			Ready:     d.Status.ReadyReplicas,
			Healthy:   d.Status.ReadyReplicas == desired,
		})
	}

	return finalizeReport(deployments), nil
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
	deployments := []DeploymentStatus{}

	for _, ns := range namespaces {
		r, err := CheckDeploymentHealth(ctx, clientset, ns)
		if err != nil {
			return DeploymentHealthReport{}, err
		}
		deployments = append(deployments, r.Deployments...)
	}

	return finalizeReport(deployments), nil
}
