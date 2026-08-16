package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	disco "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/varkey/tyk-task/golang/internal/netpolicy"
)

func int32Ptr(v int32) *int32 { return &v }

func deployment(ns, name string, replicas *int32, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: appsv1MetaWithName(ns, name),
		Spec:       appsv1.DeploymentSpec{Replicas: replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func appsv1MetaWithName(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns}
}

// authorizingFake adds TokenReview/SubjectAccessReview reactors on top of a
// clientset that already has other fixtures loaded, so auth-enabled tests
// exercise the same object set as the equivalent auth-disabled ones.
func authorizingFake(clientset *fake.Clientset, validToken string, user authenticationv1.UserInfo, allow func(authorizationv1.ResourceAttributes) bool) {
	clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		in := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		out := in.DeepCopy()
		if in.Spec.Token == validToken {
			out.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: user}
		} else {
			out.Status = authenticationv1.TokenReviewStatus{Authenticated: false, Error: "invalid token"}
		}
		return true, out, nil
	})

	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		in := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		out := in.DeepCopy()
		out.Status.Allowed = allow(*in.Spec.ResourceAttributes)
		return true, out, nil
	})
}

func TestHandleHealthz(t *testing.T) {
	s := &Server{Clientset: fake.NewSimpleClientset()}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

func TestHandleReadyz(t *testing.T) {
	t.Run("reachable API server reports ready", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		s := &Server{Clientset: clientset}

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var body map[string]any
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, true, body["ready"])
	})

	t.Run("unreachable API server reports not ready with 503", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		clientset.Discovery().(*disco.FakeDiscovery).PrependReactor("get", "version",
			func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("dial tcp: connection refused")
			})
		s := &Server{Clientset: clientset}

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

		var body map[string]any
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, false, body["ready"])
	})
}

func TestHandleDeploymentsHealth(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		deployment("ns-a", "healthy", int32Ptr(2), 2),
		deployment("ns-a", "degraded", int32Ptr(2), 1),
	)
	s := &Server{Clientset: clientset}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var report struct {
		AllHealthy       bool `json:"allHealthy"`
		TotalDeployments int  `json:"totalDeployments"`
		HealthyCount     int  `json:"healthyCount"`
		UnhealthyCount   int  `json:"unhealthyCount"`
		Deployments      []struct {
			Name    string `json:"name"`
			Healthy bool   `json:"healthy"`
		} `json:"deployments"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))
	assert.False(t, report.AllHealthy)
	assert.Len(t, report.Deployments, 2)
	assert.Equal(t, 2, report.TotalDeployments)
	assert.Equal(t, 1, report.HealthyCount)
	assert.Equal(t, 1, report.UnhealthyCount)
	// Unhealthy sorts first, so the degraded entry is scannable without
	// any client-side filtering.
	assert.Equal(t, "degraded", report.Deployments[0].Name)
	assert.False(t, report.Deployments[0].Healthy)
}

func TestHandleDeploymentsHealth_OnlyUnhealthy(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		deployment("ns-a", "healthy", int32Ptr(2), 2),
		deployment("ns-a", "degraded", int32Ptr(2), 1),
	)
	s := &Server{Clientset: clientset}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/health?onlyUnhealthy=true", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var report struct {
		AllHealthy       bool `json:"allHealthy"`
		TotalDeployments int  `json:"totalDeployments"`
		HealthyCount     int  `json:"healthyCount"`
		UnhealthyCount   int  `json:"unhealthyCount"`
		Deployments      []struct {
			Name string `json:"name"`
		} `json:"deployments"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))

	// The list is trimmed to just the unhealthy entry...
	require.Len(t, report.Deployments, 1)
	assert.Equal(t, "degraded", report.Deployments[0].Name)
	// ...but the summary counts still reflect the full set, not the
	// filtered list.
	assert.Equal(t, 2, report.TotalDeployments)
	assert.Equal(t, 1, report.HealthyCount)
	assert.Equal(t, 1, report.UnhealthyCount)
	assert.False(t, report.AllHealthy)
}

func TestHandleDeploymentsHealth_NamespaceScoped(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		deployment("ns-a", "in-scope-a", int32Ptr(1), 1),
		deployment("ns-b", "in-scope-b", int32Ptr(1), 0),
		deployment("ns-c", "out-of-scope", int32Ptr(1), 0),
	)
	s := &Server{Clientset: clientset, Namespaces: []string{"ns-a", "ns-b"}}

	t.Run("no ?namespace= aggregates across Server.Namespaces, excluding ns-c", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/health", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)

		var report struct {
			AllHealthy  bool `json:"allHealthy"`
			Deployments []struct {
				Name string `json:"name"`
			} `json:"deployments"`
		}
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))
		assert.False(t, report.AllHealthy) // in-scope-b is degraded
		require.Len(t, report.Deployments, 2)
		names := []string{report.Deployments[0].Name, report.Deployments[1].Name}
		assert.ElementsMatch(t, []string{"in-scope-a", "in-scope-b"}, names)
	})

	t.Run("an explicit ?namespace= still wins over Server.Namespaces", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/health?namespace=ns-c", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)

		var report struct {
			Deployments []struct {
				Name string `json:"name"`
			} `json:"deployments"`
		}
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))
		require.Len(t, report.Deployments, 1)
		assert.Equal(t, "out-of-scope", report.Deployments[0].Name)
	})
}

func TestHandleDeploymentsHealth_PermissionErrorMapsTo403(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "", context.DeadlineExceeded)
	})
	s := &Server{Clientset: clientset}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func isolationBody(t *testing.T, req netpolicy.IsolationRequest) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	return bytes.NewReader(b)
}

func sampleIsolationRequest() netpolicy.IsolationRequest {
	return netpolicy.IsolationRequest{
		A: netpolicy.WorkloadSelector{Namespaces: []string{"ns-a"}, MatchLabels: map[string]string{"app": "frontend"}},
		B: netpolicy.WorkloadSelector{Namespaces: []string{"ns-b"}, MatchLabels: map[string]string{"app": "backend"}},
	}
}

func TestIsolationLifecycle_AuthDisabled(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	s := &Server{Clientset: clientset, AuthEnabled: false}
	handler := s.Handler()

	// Create.
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/isolation", isolationBody(t, sampleIsolationRequest()))
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	require.Equal(t, http.StatusCreated, createRec.Code)

	var created netpolicy.Isolation
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))
	assert.NotEmpty(t, created.ID)
	assert.Len(t, created.Policies, 2)

	// List.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/isolation", nil)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	require.Equal(t, http.StatusOK, listRec.Code)

	var listed []netpolicy.Isolation
	require.NoError(t, json.NewDecoder(listRec.Body).Decode(&listed))
	require.Len(t, listed, 1)
	assert.Equal(t, created.ID, listed[0].ID)

	// Delete.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/isolation/"+created.ID, nil)
	delRec := httptest.NewRecorder()
	handler.ServeHTTP(delRec, delReq)
	assert.Equal(t, http.StatusNoContent, delRec.Code)

	// Deleting again is a 404, not a silent success.
	delAgainReq := httptest.NewRequest(http.MethodDelete, "/api/v1/isolation/"+created.ID, nil)
	delAgainRec := httptest.NewRecorder()
	handler.ServeHTTP(delAgainRec, delAgainReq)
	assert.Equal(t, http.StatusNotFound, delAgainRec.Code)
}

func TestIsolationLifecycle_NamespaceScoped(t *testing.T) {
	// Server.Namespaces set (mirroring rbac.clusterScoped=false) drives
	// List/Delete through the per-namespace path end to end, not just
	// Apply's already-namespace-scoped Create calls.
	clientset := fake.NewSimpleClientset()
	s := &Server{Clientset: clientset, AuthEnabled: false, Namespaces: []string{"ns-a", "ns-b"}}
	handler := s.Handler()

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/isolation", isolationBody(t, sampleIsolationRequest()))
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	require.Equal(t, http.StatusCreated, createRec.Code)

	var created netpolicy.Isolation
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/isolation", nil)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	require.Equal(t, http.StatusOK, listRec.Code)

	var listed []netpolicy.Isolation
	require.NoError(t, json.NewDecoder(listRec.Body).Decode(&listed))
	require.Len(t, listed, 1)
	assert.Len(t, listed[0].Policies, 2, "should find both namespaces' policies via the per-namespace loop")

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/isolation/"+created.ID, nil)
	delRec := httptest.NewRecorder()
	handler.ServeHTTP(delRec, delReq)
	assert.Equal(t, http.StatusNoContent, delRec.Code)

	remaining, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, remaining.Items, "delete should have removed the policy in both namespaces")
}

func TestHandleCreateIsolation_ValidationError(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	s := &Server{Clientset: clientset, AuthEnabled: false}

	bad := netpolicy.IsolationRequest{A: netpolicy.WorkloadSelector{Namespaces: []string{"ns-a"}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/isolation", isolationBody(t, bad))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleCreateIsolation_MalformedBody(t *testing.T) {
	s := &Server{Clientset: fake.NewSimpleClientset(), AuthEnabled: false}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/isolation", io.NopCloser(bytes.NewReader([]byte("not json"))))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestIsolation_AuthEnabled(t *testing.T) {
	validToken := "valid-token"
	user := authenticationv1.UserInfo{Username: "sre"}

	t.Run("caller with permission in both namespaces may create and delete", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		authorizingFake(clientset, validToken, user, func(attrs authorizationv1.ResourceAttributes) bool {
			return attrs.Resource == "networkpolicies" && (attrs.Namespace == "ns-a" || attrs.Namespace == "ns-b")
		})
		s := &Server{Clientset: clientset, AuthEnabled: true}
		handler := s.Handler()

		createReq := httptest.NewRequest(http.MethodPost, "/api/v1/isolation", isolationBody(t, sampleIsolationRequest()))
		createReq.Header.Set("Authorization", "Bearer "+validToken)
		createRec := httptest.NewRecorder()
		handler.ServeHTTP(createRec, createReq)
		require.Equal(t, http.StatusCreated, createRec.Code)

		var created netpolicy.Isolation
		require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

		delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/isolation/"+created.ID, nil)
		delReq.Header.Set("Authorization", "Bearer "+validToken)
		delRec := httptest.NewRecorder()
		handler.ServeHTTP(delRec, delReq)
		assert.Equal(t, http.StatusNoContent, delRec.Code)
	})

	t.Run("caller with permission in only one namespace is denied entirely", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		authorizingFake(clientset, validToken, user, func(attrs authorizationv1.ResourceAttributes) bool {
			return attrs.Namespace == "ns-a" // ns-b denied
		})
		s := &Server{Clientset: clientset, AuthEnabled: true}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/isolation", isolationBody(t, sampleIsolationRequest()))
		req.Header.Set("Authorization", "Bearer "+validToken)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)

		list, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, list.Items, "a partially-authorized request must not create anything")
	})

	t.Run("missing token is rejected before any authorization check", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		authorizingFake(clientset, validToken, user, func(authorizationv1.ResourceAttributes) bool { return true })
		s := &Server{Clientset: clientset, AuthEnabled: true}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/isolation", isolationBody(t, sampleIsolationRequest()))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("deployments health route enforces auth too", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(deployment("ns-a", "d1", int32Ptr(1), 1))
		authorizingFake(clientset, validToken, user, func(attrs authorizationv1.ResourceAttributes) bool {
			return attrs.Resource == "deployments"
		})
		s := &Server{Clientset: clientset, AuthEnabled: true}

		noAuthReq := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/health", nil)
		noAuthRec := httptest.NewRecorder()
		s.Handler().ServeHTTP(noAuthRec, noAuthReq)
		assert.Equal(t, http.StatusUnauthorized, noAuthRec.Code)

		authedReq := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/health", nil)
		authedReq.Header.Set("Authorization", "Bearer "+validToken)
		authedRec := httptest.NewRecorder()
		s.Handler().ServeHTTP(authedRec, authedReq)
		assert.Equal(t, http.StatusOK, authedRec.Code)
	})
}
