package netpolicy

import (
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// namespaceNameLabel is automatically set by the API server on every
// Namespace object (since Kubernetes 1.22) to that namespace's own name.
// Keying peer selectors off it means isolation works against namespaces
// that don't carry any custom labels of their own.
const namespaceNameLabel = "kubernetes.io/metadata.name"

// buildAllowExceptPeers returns the NetworkPolicy peer list expressing
// "allow traffic from/to anyone except pods matching excludeLabels in one of
// excludeNamespaces".
//
// A NetworkPolicy's peer list is OR'd (any matching peer permits the
// traffic), while matchExpressions within a single selector are AND'd. That
// makes it straightforward to allow everyone outside the excluded
// namespaces:
//
//	peer 1: namespaceSelector NotIn excludeNamespaces
//
// But excluding a specific labelled workload *inside* one of those
// namespaces takes more care. We need "NOT (k1=v1 AND k2=v2 AND ...)" -
// pods that fail to match at least one of the excluded workload's labels.
// By De Morgan's that's "(k1!=v1) OR (k2!=v2) OR ...", and because
// AND-of-selector but OR-of-peers is exactly what NetworkPolicy gives us,
// it's expressed as one peer per excluded label key:
//
//	peer per key: namespaceSelector = that namespace, podSelector: key NotIn [value]
//
// Getting this backwards - e.g. one selector with all keys NotIn, AND'd
// together - would instead exclude only pods that differ from the target on
// *every* label, silently leaving most of the target's actual pods
// reachable. Table-driven tests in peers_test.go pin this down for
// multi-label selectors.
func buildAllowExceptPeers(excludeNamespaces []string, excludeLabels map[string]string) []networkingv1.NetworkPolicyPeer {
	peers := []networkingv1.NetworkPolicyPeer{
		{
			NamespaceSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      namespaceNameLabel,
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   excludeNamespaces,
					},
				},
			},
		},
	}

	keys := make([]string, 0, len(excludeLabels))
	for k := range excludeLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic output, easier to test and to diff in kubectl

	for _, ns := range excludeNamespaces {
		for _, key := range keys {
			peers = append(peers, networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{namespaceNameLabel: ns},
				},
				PodSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      key,
							Operator: metav1.LabelSelectorOpNotIn,
							Values:   []string{excludeLabels[key]},
						},
					},
				},
			})
		}
	}

	return peers
}
