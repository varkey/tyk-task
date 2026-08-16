// Package authn gates API routes with Kubernetes' own identity system
// instead of a bespoke credential: TokenReview authenticates the bearer
// token presented by the caller, and SubjectAccessReview authorizes the
// resolved identity against the specific action a route is about to
// perform. See the README's "auth" section for why this shape was chosen
// over, say, a static shared secret.
//
// Every failure mode here - a missing token, a TokenReview/SAR call that
// itself errors, an explicit deny - fails closed (401/403), never open.
package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrUnauthenticated indicates the request carried no usable credential.
var ErrUnauthenticated = errors.New("authentication failed")

// Authenticate resolves the identity of the bearer token on r via
// TokenReview. It returns ErrUnauthenticated (wrapped) for a missing token,
// an invalid token, or a TokenReview call that itself fails - the caller
// gets no more detail than "authentication failed" in any of those cases,
// deliberately, so the failure mode can't be used to enumerate valid tokens.
func Authenticate(ctx context.Context, clientset kubernetes.Interface, r *http.Request) (*authenticationv1.UserInfo, error) {
	token, ok := bearerToken(r)
	if !ok {
		return nil, fmt.Errorf("%w: missing or malformed Authorization: Bearer <token> header", ErrUnauthenticated)
	}

	review, err := clientset.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: token review failed: %v", ErrUnauthenticated, err)
	}
	if !review.Status.Authenticated {
		reason := review.Status.Error
		if reason == "" {
			reason = "token not authenticated"
		}
		return nil, fmt.Errorf("%w: %s", ErrUnauthenticated, reason)
	}

	return &review.Status.User, nil
}

// Authorize checks whether user may perform the action described by attrs,
// via SubjectAccessReview. A SAR call that itself errors is treated as
// denied - the caller (the pod's own broad RBAC, doing the actual work) is
// never a fallback for "the authorization check subsystem is unavailable".
func Authorize(ctx context.Context, clientset kubernetes.Interface, user authenticationv1.UserInfo, attrs authorizationv1.ResourceAttributes) (allowed bool, reason string, err error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for k, v := range user.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}

	review, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               user.Username,
			Groups:             user.Groups,
			UID:                user.UID,
			Extra:              extra,
			ResourceAttributes: &attrs,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, "", fmt.Errorf("subject access review failed: %w", err)
	}

	return review.Status.Allowed, review.Status.Reason, nil
}

// Middleware wraps next with an Authenticate+Authorize check for routes
// whose authorization only depends on a single, statically-derivable
// ResourceAttributes (e.g. "list deployments in the namespace named by this
// query param"). Routes whose authorization depends on multiple resources
// named inside the request body (story 2's isolation endpoints, which can
// name several namespaces) call Authenticate/Authorize directly instead -
// see internal/api.
func Middleware(clientset kubernetes.Interface, attrsFor func(*http.Request) authorizationv1.ResourceAttributes) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, err := Authenticate(r.Context(), clientset, r)
			if err != nil {
				http.Error(w, ErrUnauthenticated.Error(), http.StatusUnauthorized)
				return
			}

			attrs := attrsFor(r)
			allowed, reason, err := Authorize(r.Context(), clientset, *user, attrs)
			if err != nil || !allowed {
				msg := fmt.Sprintf("%s is not permitted to %s %s", user.Username, attrs.Verb, attrs.Resource)
				if reason != "" {
					msg += ": " + reason
				}
				http.Error(w, msg, http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "

	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}

	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return token, token != ""
}
