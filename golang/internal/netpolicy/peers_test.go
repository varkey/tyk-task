package netpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func nsSelectorPeer(notIn ...string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: namespaceNameLabel, Operator: metav1.LabelSelectorOpNotIn, Values: notIn},
			},
		},
	}
}

func labelExcludePeer(ns, key, value string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{namespaceNameLabel: ns},
		},
		PodSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: key, Operator: metav1.LabelSelectorOpNotIn, Values: []string{value}},
			},
		},
	}
}

func TestBuildAllowExceptPeers(t *testing.T) {
	t.Run("single namespace, single label", func(t *testing.T) {
		got := buildAllowExceptPeers([]string{"ns-b"}, map[string]string{"app": "target"})

		want := []networkingv1.NetworkPolicyPeer{
			nsSelectorPeer("ns-b"),
			labelExcludePeer("ns-b", "app", "target"),
		}
		assert.Equal(t, want, got)
	})

	t.Run("single namespace, multiple labels produce one OR'd peer per key - not a single AND'd peer", func(t *testing.T) {
		// This is the case the doc comment in peers.go warns about: a pod
		// that matches "app=target" but not "tier=web" must still be let
		// through, because it isn't a full match for the excluded workload.
		got := buildAllowExceptPeers([]string{"ns-b"}, map[string]string{"app": "target", "tier": "web"})

		want := []networkingv1.NetworkPolicyPeer{
			nsSelectorPeer("ns-b"),
			labelExcludePeer("ns-b", "app", "target"), // keys are sorted for determinism
			labelExcludePeer("ns-b", "tier", "web"),
		}
		assert.Equal(t, want, got)
	})

	t.Run("multiple excluded namespaces", func(t *testing.T) {
		got := buildAllowExceptPeers([]string{"ns-b", "ns-c"}, map[string]string{"app": "target"})

		want := []networkingv1.NetworkPolicyPeer{
			nsSelectorPeer("ns-b", "ns-c"),
			labelExcludePeer("ns-b", "app", "target"),
			labelExcludePeer("ns-c", "app", "target"),
		}
		assert.Equal(t, want, got)
	})

	t.Run("label key order is deterministic regardless of map iteration", func(t *testing.T) {
		got1 := buildAllowExceptPeers([]string{"ns-b"}, map[string]string{"z": "1", "a": "2", "m": "3"})
		got2 := buildAllowExceptPeers([]string{"ns-b"}, map[string]string{"m": "3", "z": "1", "a": "2"})
		assert.Equal(t, got1, got2)

		keys := []string{
			got1[1].PodSelector.MatchExpressions[0].Key,
			got1[2].PodSelector.MatchExpressions[0].Key,
			got1[3].PodSelector.MatchExpressions[0].Key,
		}
		assert.Equal(t, []string{"a", "m", "z"}, keys)
	})
}
