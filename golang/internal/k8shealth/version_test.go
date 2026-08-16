package k8shealth

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	k8stesting "k8s.io/client-go/testing"

	disco "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGetKubernetesVersion(t *testing.T) {
	okClientset := fake.NewSimpleClientset()
	okClientset.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "1.25.0-fake"}

	okVer, err := GetKubernetesVersion(okClientset)
	assert.NoError(t, err)
	assert.Equal(t, "1.25.0-fake", okVer)

	badClientset := fake.NewSimpleClientset()
	badClientset.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{}

	badVer, err := GetKubernetesVersion(badClientset)
	assert.NoError(t, err)
	assert.Equal(t, "", badVer)
}

// TestGetKubernetesVersion_Unreachable covers the case story 3 actually
// cares about: the API server can't be reached at all.
func TestGetKubernetesVersion_Unreachable(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.Discovery().(*disco.FakeDiscovery).PrependReactor("get", "version",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("connection refused")
		})

	_, err := GetKubernetesVersion(clientset)
	assert.Error(t, err)
}
