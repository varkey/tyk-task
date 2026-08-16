package k8shealth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/varkey/tyk-task/golang/internal/apierr"
)

func deployment(ns, name string, replicas *int32, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       appsv1.DeploymentSpec{Replicas: replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func int32Ptr(v int32) *int32 { return &v }

func TestCheckDeploymentHealth(t *testing.T) {
	t.Run("all healthy", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			deployment("ns-a", "healthy-1", int32Ptr(3), 3),
			deployment("ns-b", "healthy-2", int32Ptr(1), 1),
		)

		report, err := CheckDeploymentHealth(context.Background(), clientset, "")
		require.NoError(t, err)

		assert.True(t, report.AllHealthy)
		assert.Len(t, report.Deployments, 2)
		assert.Equal(t, 2, report.TotalDeployments)
		assert.Equal(t, 2, report.HealthyCount)
		assert.Equal(t, 0, report.UnhealthyCount)
		for _, d := range report.Deployments {
			assert.True(t, d.Healthy)
		}
	})

	t.Run("under-replicated deployment flips AllHealthy", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			deployment("ns-a", "healthy", int32Ptr(3), 3),
			deployment("ns-a", "degraded", int32Ptr(3), 1),
		)

		report, err := CheckDeploymentHealth(context.Background(), clientset, "")
		require.NoError(t, err)

		assert.False(t, report.AllHealthy)
		assert.Equal(t, 2, report.TotalDeployments)
		assert.Equal(t, 1, report.HealthyCount)
		assert.Equal(t, 1, report.UnhealthyCount)

		byName := map[string]DeploymentStatus{}
		for _, d := range report.Deployments {
			byName[d.Name] = d
		}
		assert.True(t, byName["healthy"].Healthy)
		assert.False(t, byName["degraded"].Healthy)
		assert.Equal(t, int32(3), byName["degraded"].Desired)
		assert.Equal(t, int32(1), byName["degraded"].Ready)
	})

	t.Run("unhealthy deployments sort before healthy ones", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			deployment("ns-a", "zz-healthy", int32Ptr(1), 1),
			deployment("ns-a", "aa-healthy", int32Ptr(1), 1),
			deployment("ns-a", "zz-broken", int32Ptr(1), 0),
			deployment("ns-a", "aa-broken", int32Ptr(1), 0),
		)

		report, err := CheckDeploymentHealth(context.Background(), clientset, "")
		require.NoError(t, err)

		require.Len(t, report.Deployments, 4)
		names := make([]string, len(report.Deployments))
		for i, d := range report.Deployments {
			names[i] = d.Name
		}
		// Unhealthy first (alphabetical within the group), then healthy
		// (alphabetical within that group) - matches finalizeReport's sort.
		assert.Equal(t, []string{"aa-broken", "zz-broken", "aa-healthy", "zz-healthy"}, names)
	})

	t.Run("nil spec.Replicas defaults to 1", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(deployment("ns-a", "unset", nil, 1))

		report, err := CheckDeploymentHealth(context.Background(), clientset, "")
		require.NoError(t, err)

		require.Len(t, report.Deployments, 1)
		assert.Equal(t, int32(1), report.Deployments[0].Desired)
		assert.True(t, report.Deployments[0].Healthy)
	})

	t.Run("namespace filter narrows the list", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			deployment("ns-a", "in-scope", int32Ptr(1), 1),
			deployment("ns-b", "out-of-scope", int32Ptr(1), 0),
		)

		report, err := CheckDeploymentHealth(context.Background(), clientset, "ns-a")
		require.NoError(t, err)

		require.Len(t, report.Deployments, 1)
		assert.Equal(t, "in-scope", report.Deployments[0].Name)
		assert.True(t, report.AllHealthy)
	})

	t.Run("no deployments is trivially healthy", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()

		report, err := CheckDeploymentHealth(context.Background(), clientset, "")
		require.NoError(t, err)

		assert.True(t, report.AllHealthy)
		assert.Empty(t, report.Deployments)
	})

	t.Run("list error is propagated", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("api server unavailable")
		})

		_, err := CheckDeploymentHealth(context.Background(), clientset, "")
		assert.Error(t, err)
	})

	t.Run("a cluster-wide list denied by RBAC maps to PermissionError, not a raw 502", func(t *testing.T) {
		// This is what namespace-scoped RBAC mode actually returns for a
		// cluster-wide List - a flat denial, not a result narrowed to
		// whatever namespaces this service's Roles happen to cover.
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "", errors.New("cluster scope"))
		})

		_, err := CheckDeploymentHealth(context.Background(), clientset, "")
		require.Error(t, err)
		var perr *apierr.PermissionError
		assert.ErrorAs(t, err, &perr)
	})
}

func TestCheckDeploymentHealthAcrossNamespaces(t *testing.T) {
	t.Run("merges results across namespaces", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			deployment("ns-a", "healthy", int32Ptr(1), 1),
			deployment("ns-b", "degraded", int32Ptr(2), 1),
			deployment("ns-c", "not-in-scope", int32Ptr(1), 0), // outside the requested namespaces
		)

		report, err := CheckDeploymentHealthAcrossNamespaces(context.Background(), clientset, []string{"ns-a", "ns-b"})
		require.NoError(t, err)

		assert.False(t, report.AllHealthy)
		require.Len(t, report.Deployments, 2)
		assert.Equal(t, 2, report.TotalDeployments)
		assert.Equal(t, 1, report.HealthyCount)
		assert.Equal(t, 1, report.UnhealthyCount)
		names := []string{report.Deployments[0].Name, report.Deployments[1].Name}
		assert.ElementsMatch(t, []string{"healthy", "degraded"}, names)
		// Merging across namespaces re-sorts the combined set, so the
		// degraded deployment still lands first even though it came from
		// the second List call.
		assert.Equal(t, "degraded", report.Deployments[0].Name)
	})

	t.Run("empty namespace list is trivially healthy with an empty (not nil) array", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()

		report, err := CheckDeploymentHealthAcrossNamespaces(context.Background(), clientset, nil)
		require.NoError(t, err)
		assert.True(t, report.AllHealthy)
		assert.NotNil(t, report.Deployments)
		assert.Empty(t, report.Deployments)
	})

	t.Run("an error in any one namespace fails the whole call", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(deployment("ns-a", "healthy", int32Ptr(1), 1))
		clientset.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.GetNamespace() == "ns-b" {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "", errors.New("denied"))
			}
			return false, nil, nil
		})

		_, err := CheckDeploymentHealthAcrossNamespaces(context.Background(), clientset, []string{"ns-a", "ns-b"})
		require.Error(t, err)
		var perr *apierr.PermissionError
		assert.ErrorAs(t, err, &perr)
	})
}
