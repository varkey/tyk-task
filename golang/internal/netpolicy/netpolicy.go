package netpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/varkey/tyk-task/golang/internal/apierr"
)

const (
	managedByLabel    = "app.kubernetes.io/managed-by"
	managedByValue    = "tyk-sre-assignment"
	isolationIDLabel  = "tyk-sre-assignment/isolation-id"
	requestAnnotation = "tyk-sre-assignment/request"
)

// Apply creates (or, if already present, confirms) the NetworkPolicy objects
// that stop req.A and req.B from exchanging any network traffic, in both
// directions.
//
// It's idempotent, including with A and B swapped: calling it again with the
// same pair of workloads returns the same ID and leaves the existing
// policies untouched rather than creating duplicates. If any policy fails to
// apply partway through, everything created by this call is rolled back so
// a failed request never leaves a half-applied isolation behind.
func Apply(ctx context.Context, clientset kubernetes.Interface, req IsolationRequest) (Isolation, error) {
	req.A = normalize(req.A)
	req.B = normalize(req.B)

	if err := validate(req); err != nil {
		return Isolation{}, err
	}

	id := computeID(req.A, req.B)

	reqJSON, err := json.Marshal(req)
	if err != nil {
		return Isolation{}, fmt.Errorf("marshalling isolation request: %w", err)
	}

	var created []PolicyRef
	rollback := func() {
		for _, p := range created {
			_ = clientset.NetworkingV1().NetworkPolicies(p.Namespace).Delete(context.Background(), p.Name, metav1.DeleteOptions{})
		}
	}

	apply := func(self WorkloadSelector, other WorkloadSelector) error {
		tag := workloadTag(self)
		for _, ns := range self.Namespaces {
			np := buildNetworkPolicy(id, tag, ns, self.MatchLabels, other.Namespaces, other.MatchLabels, string(reqJSON))
			if err := applyOne(ctx, clientset, np, id); err != nil {
				rollback()
				return err
			}
			created = append(created, PolicyRef{Namespace: np.Namespace, Name: np.Name})
		}
		return nil
	}

	if err := apply(req.A, req.B); err != nil {
		return Isolation{}, err
	}
	if err := apply(req.B, req.A); err != nil {
		return Isolation{}, err
	}

	return Isolation{ID: id, A: req.A, B: req.B, Policies: created}, nil
}

// List returns every isolation currently applied by this tool, discovered
// from the NetworkPolicy objects themselves (grouped by isolation ID) rather
// than from any separate store - the cluster is the source of truth.
//
// namespaces mirrors the chart's rbac.namespaces: empty means cluster-wide
// (one List call, requires the ClusterRole), non-empty means one List call
// per namespace instead, merged - a cluster-wide List is flatly denied by
// RBAC when this service only holds namespace-scoped Roles, it doesn't get
// silently narrowed to what those Roles cover.
func List(ctx context.Context, clientset kubernetes.Interface, namespaces []string) ([]Isolation, error) {
	items, err := listManagedPolicies(ctx, clientset, namespaces, fmt.Sprintf("%s=%s", managedByLabel, managedByValue))
	if err != nil {
		return nil, err
	}

	byID := map[string]*Isolation{}
	var order []string

	for _, np := range items {
		id := np.Labels[isolationIDLabel]
		if id == "" {
			continue
		}

		iso, ok := byID[id]
		if !ok {
			iso = &Isolation{ID: id}
			if raw, ok := np.Annotations[requestAnnotation]; ok {
				var req IsolationRequest
				if jsonErr := json.Unmarshal([]byte(raw), &req); jsonErr == nil {
					iso.A, iso.B = req.A, req.B
				}
			}
			byID[id] = iso
			order = append(order, id)
		}

		iso.Policies = append(iso.Policies, PolicyRef{Namespace: np.Namespace, Name: np.Name})
	}

	result := make([]Isolation, 0, len(order))
	for _, id := range order {
		result = append(result, *byID[id])
	}
	return result, nil
}

// Delete reverses the isolation with the given ID, removing every
// NetworkPolicy it created. Returns a *apierr.NotFoundError if no isolation
// with that ID exists. namespaces has the same meaning as in List.
func Delete(ctx context.Context, clientset kubernetes.Interface, id string, namespaces []string) error {
	items, err := listManagedPolicies(ctx, clientset, namespaces, fmt.Sprintf("%s=%s", isolationIDLabel, id))
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return &apierr.NotFoundError{Msg: "no isolation found with id " + id}
	}

	var errs []error
	for _, np := range items {
		if err := clientset.NetworkingV1().NetworkPolicies(np.Namespace).Delete(ctx, np.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			if apierrors.IsForbidden(err) {
				errs = append(errs, &apierr.PermissionError{Msg: fmt.Sprintf("not permitted to delete networkpolicies in namespace %q", np.Namespace)})
				continue
			}
			errs = append(errs, fmt.Errorf("deleting %s/%s: %w", np.Namespace, np.Name, err))
		}
	}
	return errors.Join(errs...)
}

// listManagedPolicies lists NetworkPolicies matching labelSelector, either
// with a single cluster-wide call (namespaces empty) or one call per
// namespace, merged (namespaces non-empty) - see List's doc comment for why
// the two aren't interchangeable under namespace-scoped RBAC.
func listManagedPolicies(ctx context.Context, clientset kubernetes.Interface, namespaces []string, labelSelector string) ([]networkingv1.NetworkPolicy, error) {
	if len(namespaces) == 0 {
		list, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return nil, wrapListError(err, "")
		}
		return list.Items, nil
	}

	var items []networkingv1.NetworkPolicy
	for _, ns := range namespaces {
		list, err := clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return nil, wrapListError(err, ns)
		}
		items = append(items, list.Items...)
	}
	return items, nil
}

func wrapListError(err error, namespace string) error {
	if apierrors.IsForbidden(err) {
		if namespace == "" {
			return &apierr.PermissionError{Msg: "not permitted to list networkpolicies cluster-wide"}
		}
		return &apierr.PermissionError{Msg: fmt.Sprintf("not permitted to list networkpolicies in namespace %q", namespace)}
	}
	return fmt.Errorf("listing networkpolicies: %w", err)
}

// applyOne creates np, treating "already exists with our isolation-id label"
// as success (idempotent re-apply) and any other conflict as an error.
func applyOne(ctx context.Context, clientset kubernetes.Interface, np *networkingv1.NetworkPolicy, id string) error {
	_, err := clientset.NetworkingV1().NetworkPolicies(np.Namespace).Create(ctx, np, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	// A namespace that doesn't exist surfaces here as a 404 from the
	// Create call itself (the apiserver rejects it at admission) - no
	// separate "get/list namespaces" permission is needed to catch this,
	// just to map it to a caller-facing ValidationError (400) instead of
	// letting it fall through as an opaque 502.
	if apierrors.IsNotFound(err) {
		return &apierr.ValidationError{Msg: fmt.Sprintf("namespace %q does not exist", np.Namespace)}
	}
	// In namespace-scoped RBAC mode, a request naming a namespace outside
	// the configured set is rejected by the apiserver itself with this -
	// this service's own RBAC is the enforcement, not any app-level check.
	if apierrors.IsForbidden(err) {
		return &apierr.PermissionError{Msg: fmt.Sprintf("not permitted to create networkpolicies in namespace %q", np.Namespace)}
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating networkpolicy %s/%s: %w", np.Namespace, np.Name, err)
	}

	existing, getErr := clientset.NetworkingV1().NetworkPolicies(np.Namespace).Get(ctx, np.Name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("networkpolicy %s/%s already exists but could not be read: %w", np.Namespace, np.Name, getErr)
	}
	if existing.Labels[isolationIDLabel] != id {
		return fmt.Errorf("networkpolicy %s/%s already exists and is not managed by this isolation request", np.Namespace, np.Name)
	}
	return nil
}

func buildNetworkPolicy(id, tag, namespace string, podSelector map[string]string, excludeNamespaces []string, excludeLabels map[string]string, requestJSON string) *networkingv1.NetworkPolicy {
	peers := buildAllowExceptPeers(excludeNamespaces, excludeLabels)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("tyk-isolate-%s-%s", id, tag),
			Namespace: namespace,
			Labels: map[string]string{
				managedByLabel:   managedByValue,
				isolationIDLabel: id,
			},
			Annotations: map[string]string{
				requestAnnotation: requestJSON,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podSelector},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: peers}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: peers}},
		},
	}
}

// normalize dedupes and sorts a selector's namespaces so that requests
// differing only in ordering/duplication produce the same isolation ID and
// don't attempt duplicate object creation.
func normalize(w WorkloadSelector) WorkloadSelector {
	seen := map[string]bool{}
	ns := make([]string, 0, len(w.Namespaces))
	for _, n := range w.Namespaces {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		ns = append(ns, n)
	}
	sort.Strings(ns)
	w.Namespaces = ns
	return w
}

func validate(req IsolationRequest) error {
	if len(req.A.Namespaces) == 0 || len(req.B.Namespaces) == 0 {
		return &apierr.ValidationError{Msg: "both workloads must specify at least one namespace"}
	}
	if len(req.A.MatchLabels) == 0 || len(req.B.MatchLabels) == 0 {
		return &apierr.ValidationError{Msg: "both workloads must specify at least one label in matchLabels"}
	}
	if canonical(req.A) == canonical(req.B) {
		return &apierr.ValidationError{Msg: "the two workloads must be different - a workload can't be isolated from itself"}
	}
	return nil
}

// computeID derives a deterministic, order-independent ID for a pair of
// workload selectors, so isolating (A, B) and (B, A) produce the same ID and
// requesting the same isolation twice is a no-op rather than a duplicate.
func computeID(a, b WorkloadSelector) string {
	sa, sb := canonical(a), canonical(b)
	if sa > sb {
		sa, sb = sb, sa
	}
	sum := sha256.Sum256([]byte(sa + "|" + sb))
	return hex.EncodeToString(sum[:])[:12]
}

// workloadTag derives a short, deterministic identifier for a single
// workload selector, used as a NetworkPolicy name suffix so the two
// policies from one isolation never collide - including when A and B share
// a namespace - and so it's independent of whether the caller passed a
// given workload as req.A or req.B (Apply(A, B) and Apply(B, A) must create
// identical objects, not four objects instead of two).
func workloadTag(w WorkloadSelector) string {
	sum := sha256.Sum256([]byte(canonical(w)))
	return hex.EncodeToString(sum[:])[:8]
}

func canonical(w WorkloadSelector) string {
	keys := make([]string, 0, len(w.MatchLabels))
	for k := range w.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(strings.Join(w.Namespaces, ","))
	b.WriteString(";")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s,", k, w.MatchLabels[k])
	}
	return b.String()
}
