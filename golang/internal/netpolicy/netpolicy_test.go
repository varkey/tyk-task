package netpolicy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/varkey/tyk-task/golang/internal/apierr"
)

func unmanagedPolicy(name, ns string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
}

func sample() IsolationRequest {
	return IsolationRequest{
		A: WorkloadSelector{Namespaces: []string{"ns-a"}, MatchLabels: map[string]string{"app": "frontend"}},
		B: WorkloadSelector{Namespaces: []string{"ns-b"}, MatchLabels: map[string]string{"app": "backend"}},
	}
}

func TestApply(t *testing.T) {
	t.Run("creates a symmetric policy pair", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()

		iso, err := Apply(context.Background(), clientset, sample())
		require.NoError(t, err)
		assert.NotEmpty(t, iso.ID)
		assert.Len(t, iso.Policies, 2)

		list, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, list.Items, 2)

		byNamespace := map[string]int{}
		for _, np := range list.Items {
			byNamespace[np.Namespace]++
			assert.Equal(t, managedByValue, np.Labels[managedByLabel])
			assert.Equal(t, iso.ID, np.Labels[isolationIDLabel])
			assert.Contains(t, np.Annotations, requestAnnotation)
		}
		assert.Equal(t, 1, byNamespace["ns-a"])
		assert.Equal(t, 1, byNamespace["ns-b"])
	})

	t.Run("is idempotent for repeated identical requests", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()

		first, err := Apply(context.Background(), clientset, sample())
		require.NoError(t, err)

		second, err := Apply(context.Background(), clientset, sample())
		require.NoError(t, err)

		assert.Equal(t, first.ID, second.ID)

		list, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, list.Items, 2, "re-applying the same isolation must not create duplicate policies")
	})

	t.Run("is idempotent with A and B swapped", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		req := sample()

		first, err := Apply(context.Background(), clientset, req)
		require.NoError(t, err)

		swapped := IsolationRequest{A: req.B, B: req.A}
		second, err := Apply(context.Background(), clientset, swapped)
		require.NoError(t, err)

		assert.Equal(t, first.ID, second.ID, "isolation ID must not depend on argument order")

		list, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, list.Items, 2, "swapping A and B must not create duplicate policies")
	})

	t.Run("handles A and B sharing a namespace without name collisions", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		req := IsolationRequest{
			A: WorkloadSelector{Namespaces: []string{"shared"}, MatchLabels: map[string]string{"app": "frontend"}},
			B: WorkloadSelector{Namespaces: []string{"shared"}, MatchLabels: map[string]string{"app": "backend"}},
		}

		iso, err := Apply(context.Background(), clientset, req)
		require.NoError(t, err)
		require.Len(t, iso.Policies, 2)
		assert.NotEqual(t, iso.Policies[0].Name, iso.Policies[1].Name)

		list, err := clientset.NetworkingV1().NetworkPolicies("shared").List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, list.Items, 2)
	})

	t.Run("rejects isolating a workload from itself", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		w := WorkloadSelector{Namespaces: []string{"ns-a"}, MatchLabels: map[string]string{"app": "frontend"}}

		_, err := Apply(context.Background(), clientset, IsolationRequest{A: w, B: w})
		require.Error(t, err)
		var verr *apierr.ValidationError
		assert.ErrorAs(t, err, &verr)
	})

	t.Run("rejects missing namespaces or labels", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()

		cases := []IsolationRequest{
			{A: WorkloadSelector{MatchLabels: map[string]string{"app": "a"}}, B: WorkloadSelector{Namespaces: []string{"ns-b"}, MatchLabels: map[string]string{"app": "b"}}},
			{A: WorkloadSelector{Namespaces: []string{"ns-a"}}, B: WorkloadSelector{Namespaces: []string{"ns-b"}, MatchLabels: map[string]string{"app": "b"}}},
		}
		for _, req := range cases {
			_, err := Apply(context.Background(), clientset, req)
			require.Error(t, err)
			var verr *apierr.ValidationError
			assert.ErrorAs(t, err, &verr)
		}
	})

	t.Run("a namespace that doesn't exist is a clean ValidationError, not a raw API error", func(t *testing.T) {
		// No separate "get/list namespaces" permission is used or needed for
		// this - the apiserver itself rejects Create into a missing
		// namespace with a 404, which applyOne maps to a ValidationError.
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("create", "networkpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
			ns := action.GetNamespace()
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
		})

		_, err := Apply(context.Background(), clientset, sample())
		require.Error(t, err)
		var verr *apierr.ValidationError
		require.ErrorAs(t, err, &verr)
		assert.Contains(t, err.Error(), "does not exist")
	})

	t.Run("multiple namespaces per workload creates one policy per namespace", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		req := IsolationRequest{
			A: WorkloadSelector{Namespaces: []string{"ns-a1", "ns-a2"}, MatchLabels: map[string]string{"app": "frontend"}},
			B: WorkloadSelector{Namespaces: []string{"ns-b"}, MatchLabels: map[string]string{"app": "backend"}},
		}

		iso, err := Apply(context.Background(), clientset, req)
		require.NoError(t, err)
		assert.Len(t, iso.Policies, 3)
	})
}

func TestListAndDelete(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	iso, err := Apply(context.Background(), clientset, sample())
	require.NoError(t, err)

	list, err := List(context.Background(), clientset, nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, iso.ID, list[0].ID)
	assert.Equal(t, sample().A, list[0].A)
	assert.Equal(t, sample().B, list[0].B)
	assert.Len(t, list[0].Policies, 2)

	require.NoError(t, Delete(context.Background(), clientset, iso.ID, nil))

	remaining, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, remaining.Items)

	list, err = List(context.Background(), clientset, nil)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestDelete_NotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	err := Delete(context.Background(), clientset, "does-not-exist", nil)
	require.Error(t, err)
	var nferr *apierr.NotFoundError
	assert.ErrorAs(t, err, &nferr)
}

func TestList_IgnoresUnmanagedPolicies(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	_, err := Apply(context.Background(), clientset, sample())
	require.NoError(t, err)

	// A NetworkPolicy that pre-existed and has nothing to do with this tool
	// must not show up in List, nor be touched by Delete.
	_, err = clientset.NetworkingV1().NetworkPolicies("ns-a").Create(context.Background(), unmanagedPolicy("other-policy", "ns-a"), metav1.CreateOptions{})
	require.NoError(t, err)

	list, err := List(context.Background(), clientset, nil)
	require.NoError(t, err)
	require.Len(t, list, 1)

	after, err := clientset.NetworkingV1().NetworkPolicies("ns-a").Get(context.Background(), "other-policy", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotNil(t, after)
}

func TestListAndDelete_NamespaceScoped(t *testing.T) {
	// Namespace-scoped mode has to get the same result as cluster-wide mode
	// by issuing one List/Delete per configured namespace instead of a
	// single cluster-wide call - this pins down that the per-namespace
	// results are merged correctly, not just that each call succeeds.
	clientset := fake.NewSimpleClientset()

	iso, err := Apply(context.Background(), clientset, sample()) // ns-a + ns-b
	require.NoError(t, err)

	t.Run("List merges across the configured namespaces", func(t *testing.T) {
		list, err := List(context.Background(), clientset, []string{"ns-a", "ns-b"})
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, iso.ID, list[0].ID)
		assert.Len(t, list[0].Policies, 2, "should find the policy from both namespaces, not just one")
	})

	t.Run("List only sees namespaces it's told about", func(t *testing.T) {
		list, err := List(context.Background(), clientset, []string{"ns-a"})
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Len(t, list[0].Policies, 1, "ns-b's half of the isolation is out of scope")
	})

	t.Run("Delete removes the policy in every configured namespace", func(t *testing.T) {
		require.NoError(t, Delete(context.Background(), clientset, iso.ID, []string{"ns-a", "ns-b"}))

		remaining, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, remaining.Items)
	})
}

func TestListAndDelete_PermissionError(t *testing.T) {
	forbidden := func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "networkpolicies"}, "", context.DeadlineExceeded)
	}

	t.Run("List, cluster-wide", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("list", "networkpolicies", forbidden)

		_, err := List(context.Background(), clientset, nil)
		require.Error(t, err)
		var perr *apierr.PermissionError
		assert.ErrorAs(t, err, &perr)
	})

	t.Run("List, namespace-scoped", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("list", "networkpolicies", forbidden)

		_, err := List(context.Background(), clientset, []string{"ns-a"})
		require.Error(t, err)
		var perr *apierr.PermissionError
		assert.ErrorAs(t, err, &perr)
	})

	t.Run("Apply, denied in one namespace", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("create", "networkpolicies", forbidden)

		_, err := Apply(context.Background(), clientset, sample())
		require.Error(t, err)
		var perr *apierr.PermissionError
		assert.ErrorAs(t, err, &perr)
	})
}
