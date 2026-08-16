package authn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const validToken = "valid-token"

var testUser = authenticationv1.UserInfo{Username: "alice", Groups: []string{"sre-team"}}

// fakeAuthCluster wires TokenReview/SubjectAccessReview reactors, since the
// fake clientset's default behaviour just echoes the request object back
// with an empty Status (i.e. always "not authenticated" / "not allowed"),
// which is convenient for the deny-path tests but needs overriding to
// exercise the allow path.
func fakeAuthCluster(allowedVerbResource map[string]bool) *fake.Clientset {
	clientset := fake.NewSimpleClientset()

	clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		in := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		out := in.DeepCopy()
		if in.Spec.Token == validToken {
			out.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: testUser}
		} else {
			out.Status = authenticationv1.TokenReviewStatus{Authenticated: false, Error: "invalid bearer token"}
		}
		return true, out, nil
	})

	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		in := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		out := in.DeepCopy()
		key := in.Spec.ResourceAttributes.Verb + ":" + in.Spec.ResourceAttributes.Resource
		out.Status.Allowed = allowedVerbResource[key]
		if !out.Status.Allowed {
			out.Status.Reason = "denied by test fixture"
		}
		return true, out, nil
	})

	return clientset
}

func TestAuthenticate(t *testing.T) {
	clientset := fakeAuthCluster(nil)

	t.Run("valid token resolves the identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+validToken)

		user, err := Authenticate(context.Background(), clientset, req)
		require.NoError(t, err)
		assert.Equal(t, "alice", user.Username)
	})

	t.Run("missing header fails closed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)

		_, err := Authenticate(context.Background(), clientset, req)
		assert.ErrorIs(t, err, ErrUnauthenticated)
	})

	t.Run("wrong token fails closed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer garbage")

		_, err := Authenticate(context.Background(), clientset, req)
		assert.ErrorIs(t, err, ErrUnauthenticated)
	})

	t.Run("non-bearer scheme fails closed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")

		_, err := Authenticate(context.Background(), clientset, req)
		assert.ErrorIs(t, err, ErrUnauthenticated)
	})

	t.Run("TokenReview call error fails closed, not open", func(t *testing.T) {
		broken := fake.NewSimpleClientset()
		broken.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unavailable")
		})

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+validToken)

		_, err := Authenticate(context.Background(), broken, req)
		assert.ErrorIs(t, err, ErrUnauthenticated)
	})
}

func TestAuthorize(t *testing.T) {
	clientset := fakeAuthCluster(map[string]bool{"list:deployments": true})

	t.Run("allowed action", func(t *testing.T) {
		allowed, _, err := Authorize(context.Background(), clientset, testUser, authorizationv1.ResourceAttributes{
			Verb: "list", Resource: "deployments",
		})
		require.NoError(t, err)
		assert.True(t, allowed)
	})

	t.Run("denied action carries a reason", func(t *testing.T) {
		allowed, reason, err := Authorize(context.Background(), clientset, testUser, authorizationv1.ResourceAttributes{
			Verb: "delete", Resource: "networkpolicies",
		})
		require.NoError(t, err)
		assert.False(t, allowed)
		assert.NotEmpty(t, reason)
	})

	t.Run("SubjectAccessReview call error is treated as denied, not allowed", func(t *testing.T) {
		broken := fake.NewSimpleClientset()
		broken.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unavailable")
		})

		allowed, _, err := Authorize(context.Background(), broken, testUser, authorizationv1.ResourceAttributes{
			Verb: "list", Resource: "deployments",
		})
		assert.Error(t, err)
		assert.False(t, allowed)
	})
}

func TestMiddleware(t *testing.T) {
	clientset := fakeAuthCluster(map[string]bool{"list:deployments": true})

	attrsFor := func(r *http.Request) authorizationv1.ResourceAttributes {
		return authorizationv1.ResourceAttributes{Verb: "list", Resource: "deployments"}
	}

	handlerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := Middleware(clientset, attrsFor)(inner)

	t.Run("authenticated and authorized reaches the handler", func(t *testing.T) {
		handlerCalled = false
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+validToken)
		rec := httptest.NewRecorder()

		mw.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, handlerCalled)
	})

	t.Run("no token is rejected with 401 before reaching the handler", func(t *testing.T) {
		handlerCalled = false
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()

		mw.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.False(t, handlerCalled)
	})

	t.Run("authenticated but unauthorized is rejected with 403", func(t *testing.T) {
		deniedMW := Middleware(clientset, func(r *http.Request) authorizationv1.ResourceAttributes {
			return authorizationv1.ResourceAttributes{Verb: "delete", Resource: "networkpolicies"}
		})(inner)

		handlerCalled = false
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+validToken)
		rec := httptest.NewRecorder()

		deniedMW.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.False(t, handlerCalled)
	})
}
