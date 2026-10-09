package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

const kubeSystemUID = "a853062b-a841-4c7a-b3f6-946cc27abb46"

func kubeSystem() *unstructured.Unstructured {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName("kube-system")
	ns.SetUID(kubeSystemUID)
	return ns
}

func fakeCluster(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{}
	for _, r := range watched {
		listKinds[r.gvr] = r.listKind
	}
	listKinds[namespaces] = "NamespaceList"
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds,
		objects...)
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func watchesStarted(c *dynamicfake.FakeDynamicClient) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == "list" || a.GetVerb() == "watch" {
			n++
		}
	}
	return n
}

func TestAMissingURLOrKeyIsAUsageErrorBeforeAnyWatch(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"no URL": {"HOROPTER_INGRESS_KEY": "hpk_x"},
		"no key": {"HOROPTER_INGRESS_URL": "http://ingress"},
	} {
		t.Run(name, func(t *testing.T) {
			c := fakeCluster(kubeSystem())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := run(ctx, env(vars), c)

			var usage usageError
			require.ErrorAs(t, err, &usage)
			assert.Zero(t, watchesStarted(c))
		})
	}
}

func TestAnUnreadableKubeSystemNamespaceFailsBeforeAnyWatch(t *testing.T) {
	c := fakeCluster(kubeSystem())
	c.PrependReactor("get", "namespaces",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(namespaces.GroupResource(), "kube-system",
				errors.New("RBAC: access denied"))
		})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := run(ctx, env(map[string]string{
		"HOROPTER_INGRESS_URL": "http://ingress", "HOROPTER_INGRESS_KEY": "hpk_x",
	}), c)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-system")
	assert.Zero(t, watchesStarted(c))
}
