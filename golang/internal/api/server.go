// Package api wires all five user stories onto a single net/http server:
// story 3's connectivity check, story 1's deployment health report, and
// story 2's on-demand network isolation, each optionally gated by the
// TokenReview/SubjectAccessReview middleware in internal/authn.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/varkey/tyk-task/golang/internal/apierr"
	"github.com/varkey/tyk-task/golang/internal/authn"
	"github.com/varkey/tyk-task/golang/internal/k8shealth"
	"github.com/varkey/tyk-task/golang/internal/netpolicy"
)

// Server holds the dependencies shared by every route.
type Server struct {
	Clientset kubernetes.Interface

	// AuthEnabled gates the /api/v1 routes behind TokenReview/
	// SubjectAccessReview. /healthz and /readyz are never gated - kubelet's
	// probe requests don't carry any of this application's credentials, and
	// neither endpoint reveals anything more sensitive than "is this pod up"
	// / "can this pod reach the API server".
	AuthEnabled bool

	// Namespaces mirrors the chart's rbac.namespaces: empty (the default,
	// paired with rbac.clusterScoped=true) means deployment health and
	// isolation list/delete each do a single cluster-wide call. Non-empty
	// means one call per listed namespace instead, merged - a cluster-wide
	// List/Get is flatly denied by RBAC when this service only holds
	// namespace-scoped Roles rather than narrowed to what they cover, so
	// this is required for namespace-scoped RBAC mode to work at all, not
	// just an optimization.
	Namespaces []string
}

// Handler builds the complete request router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	mux.Handle("GET /api/v1/deployments/health", s.protect(s.handleDeploymentsHealth, deploymentsHealthAttrs))
	mux.Handle("GET /api/v1/isolation", s.protect(s.handleListIsolations, listIsolationsAttrs))
	mux.HandleFunc("POST /api/v1/isolation", s.handleCreateIsolation)
	mux.HandleFunc("DELETE /api/v1/isolation/{id}", s.handleDeleteIsolation)

	return mux
}

// protect wraps routes whose authorization is a single, statically
// derivable check. The isolation create/delete routes authorize per
// namespace named in the request instead - see authorizeNamespaces.
func (s *Server) protect(h http.HandlerFunc, attrsFor func(*http.Request) authorizationv1.ResourceAttributes) http.Handler {
	if !s.AuthEnabled {
		return h
	}
	return authn.Middleware(s.Clientset, attrsFor)(h)
}

func deploymentsHealthAttrs(r *http.Request) authorizationv1.ResourceAttributes {
	return authorizationv1.ResourceAttributes{
		Verb:      "list",
		Group:     "apps",
		Resource:  "deployments",
		Namespace: r.URL.Query().Get("namespace"),
	}
}

func listIsolationsAttrs(*http.Request) authorizationv1.ResourceAttributes {
	return authorizationv1.ResourceAttributes{Verb: "list", Group: "networking.k8s.io", Resource: "networkpolicies"}
}

// handleHealthz is a pure liveness probe: it never touches the Kubernetes
// API, only reports the process itself is up.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("ok")); err != nil {
		fmt.Println("failed writing to response")
	}
}

// handleReadyz answers story 3: can this tool currently reach the
// configured Kubernetes API server. It performs a live call each time
// (bounded by the --k8s-timeout used to build the clientset) rather than
// caching a previous result, so it always reflects the current connection
// state - and, wired as the Deployment's readinessProbe, surfaces that
// state natively via `kubectl get pods`.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	kubeVersion, err := k8shealth.GetKubernetesVersion(s.Clientset)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready": false,
			"error": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ready":             true,
		"kubernetesVersion": kubeVersion,
	})
}

// handleDeploymentsHealth answers story 1. An explicit ?namespace= always
// wins; otherwise it follows s.Namespaces - see that field's doc comment.
func (s *Server) handleDeploymentsHealth(w http.ResponseWriter, r *http.Request) {
	var report k8shealth.DeploymentHealthReport
	var err error

	switch ns := r.URL.Query().Get("namespace"); {
	case ns != "":
		report, err = k8shealth.CheckDeploymentHealth(r.Context(), s.Clientset, ns)
	case len(s.Namespaces) > 0:
		report, err = k8shealth.CheckDeploymentHealthAcrossNamespaces(r.Context(), s.Clientset, s.Namespaces)
	default:
		report, err = k8shealth.CheckDeploymentHealth(r.Context(), s.Clientset, "")
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, report)
}

// handleCreateIsolation answers story 2's "on demand" half: enact an
// isolation between two workloads. Authorization is checked per namespace
// named in the request body, since a static route-level check can't express
// "may create in ns-a and ns-b" for namespaces only known once the body is
// parsed.
func (s *Server) handleCreateIsolation(w http.ResponseWriter, r *http.Request) {
	var req netpolicy.IsolationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	if s.AuthEnabled {
		user, err := authn.Authenticate(r.Context(), s.Clientset, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if err := s.authorizeNamespaces(r.Context(), *user, "create", req.A.Namespaces, req.B.Namespaces); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}

	iso, err := netpolicy.Apply(r.Context(), s.Clientset, req)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, iso)
}

// handleListIsolations answers "what's currently isolated" - useful both
// for the on-demand delete flow below and for demoing the story.
func (s *Server) handleListIsolations(w http.ResponseWriter, r *http.Request) {
	isolations, err := netpolicy.List(r.Context(), s.Clientset, s.Namespaces)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, isolations)
}

// handleDeleteIsolation answers story 2's "on demand" other half: reversing
// a previously applied isolation. The namespaces to authorize against
// aren't known until the isolation is looked up by ID, so this looks it up
// first (a read against the tool's own broad RBAC, not gated per-caller -
// it discloses nothing the caller didn't already name in the URL) and only
// then checks the caller's own permission to delete in each namespace
// involved.
func (s *Server) handleDeleteIsolation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var user *authenticationv1.UserInfo
	if s.AuthEnabled {
		u, err := authn.Authenticate(r.Context(), s.Clientset, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		user = u
	}

	isolations, err := netpolicy.List(r.Context(), s.Clientset, s.Namespaces)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	var target *netpolicy.Isolation
	for i := range isolations {
		if isolations[i].ID == id {
			target = &isolations[i]
			break
		}
	}
	if target == nil {
		http.Error(w, fmt.Sprintf("no isolation found with id %q", id), http.StatusNotFound)
		return
	}

	if s.AuthEnabled {
		if err := s.authorizeNamespaces(r.Context(), *user, "delete", target.A.Namespaces, target.B.Namespaces); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}

	if err := netpolicy.Delete(r.Context(), s.Clientset, id, s.Namespaces); err != nil {
		writeAPIError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// authorizeNamespaces requires that user may perform verb on networkpolicies
// in every namespace across the given lists, deduplicated. All must be
// allowed; the first denial (or SAR error - fails closed) stops the check.
func (s *Server) authorizeNamespaces(ctx context.Context, user authenticationv1.UserInfo, verb string, namespaceLists ...[]string) error {
	seen := map[string]bool{}
	for _, list := range namespaceLists {
		for _, ns := range list {
			if seen[ns] {
				continue
			}
			seen[ns] = true

			allowed, reason, err := authn.Authorize(ctx, s.Clientset, user, authorizationv1.ResourceAttributes{
				Verb: verb, Group: "networking.k8s.io", Resource: "networkpolicies", Namespace: ns,
			})
			if err != nil || !allowed {
				msg := fmt.Sprintf("%s is not permitted to %s networkpolicies in namespace %q", user.Username, verb, ns)
				if reason != "" {
					msg += ": " + reason
				}
				return errors.New(msg)
			}
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAPIError maps the shared apierr types - returned by both
// internal/k8shealth and internal/netpolicy - to the right HTTP status.
// Anything else (an error this service didn't specifically recognize) falls
// through to 502, since it means the Kubernetes API call itself failed in
// some way neither package accounts for.
func writeAPIError(w http.ResponseWriter, err error) {
	var verr *apierr.ValidationError
	if errors.As(err, &verr) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var nferr *apierr.NotFoundError
	if errors.As(err, &nferr) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	var perr *apierr.PermissionError
	if errors.As(err, &perr) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	http.Error(w, err.Error(), http.StatusBadGateway)
}
