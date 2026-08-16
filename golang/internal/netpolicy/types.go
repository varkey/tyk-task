// Package netpolicy implements story 2: cutting off network traffic between
// two workloads, identified by namespace(s) and a pod label selector, on
// demand.
//
// Vanilla Kubernetes NetworkPolicy is allow-only - multiple policies
// selecting the same pods union their permitted traffic, they never
// subtract from it, so there's no way to bolt a "deny this one peer" policy
// on top of whatever else already allows traffic to a workload. Instead,
// for each workload this package generates the complete allowed-traffic
// policy as "allow everyone except the other workload", expressed with
// NotIn match expressions. See the peers.go doc comment for the construction.
package netpolicy

// WorkloadSelector identifies a workload as a set of namespaces plus an
// exact-match pod label selector. Only matchLabels-style selectors are
// supported (no matchExpressions) - see peers.go for why that keeps the
// negation used to build the isolation policies correct.
type WorkloadSelector struct {
	Namespaces  []string          `json:"namespaces"`
	MatchLabels map[string]string `json:"matchLabels"`
}

// IsolationRequest names the two workloads that should not be able to
// exchange any network traffic.
type IsolationRequest struct {
	A WorkloadSelector `json:"a"`
	B WorkloadSelector `json:"b"`
}

// PolicyRef identifies one NetworkPolicy object created to enforce an
// isolation.
type PolicyRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Isolation is the applied state of an isolation request.
type Isolation struct {
	ID       string           `json:"id"`
	A        WorkloadSelector `json:"a"`
	B        WorkloadSelector `json:"b"`
	Policies []PolicyRef      `json:"policies"`
}
