/*/
Copyright 2022 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apibinding

import (
	"context"
	"fmt"
	"testing"
	"time"

	kcpkubernetesclientset "github.com/kcp-dev/client-go/kubernetes"
	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/apis/third_party/conditions/util/conditions"
	kcpclientset "github.com/kcp-dev/kcp/pkg/client/clientset/versioned/cluster"
	"github.com/kcp-dev/kcp/test/e2e/fixtures/apifixtures"
	"github.com/kcp-dev/kcp/test/e2e/framework"
)

func TestPermissionClaimsByName(t *testing.T) {
	t.Parallel()
	framework.Suite(t, "control-plane")

	server := framework.SharedKcpServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	orgClusterName, _ := framework.NewOrganizationFixture(t, server)
	_, serviceProviderWorkspace := framework.NewWorkspaceFixture(t, server, orgClusterName)
	_, consumerWorkspace := framework.NewWorkspaceFixture(t, server, orgClusterName)

	// Use the cluster hash since we're not using the front proxy here
	serviceProviderPath := logicalcluster.NewPath(serviceProviderWorkspace.Spec.Cluster)
	consumerPath := logicalcluster.NewPath(consumerWorkspace.Spec.Cluster)

	t.Logf("provider workspace: %s", serviceProviderPath)
	t.Logf("consumer workspace: %s", consumerPath)

	cfg := server.BaseConfig(t)

	kcpClusterClient, err := kcpclientset.NewForConfig(cfg)
	require.NoError(t, err, "failed to construct kcp cluster client for server")

	kubeClusterClient, err := kcpkubernetesclientset.NewForConfig(cfg)
	require.NoError(t, err, "failed to construct kube cluster client for server")

	t.Logf("installing a sheriff APIResourceSchema and APIExport into workspace %q", serviceProviderPath)
	apifixtures.CreateSheriffsSchemaAndExport(ctx, t, serviceProviderPath, kcpClusterClient, "wild.wild.west", "board the wanderer")

	sheriffExport := &apisv1alpha1.APIExport{}
	framework.Eventually(t, func() (done bool, str string) {
		sheriffExport, err = kcpClusterClient.Cluster(serviceProviderPath).ApisV1alpha1().APIExports().Get(ctx, "wild.wild.west", metav1.GetOptions{})
		if err != nil {
			return false, err.Error()
		}

		if conditions.IsTrue(sheriffExport, apisv1alpha1.APIExportIdentityValid) {
			return true, ""
		}
		condition := conditions.Get(sheriffExport, apisv1alpha1.APIExportIdentityValid)
		if condition != nil {
			return false, fmt.Sprintf("not done waiting for API Export condition status:%v - reason: %v - message: %v", condition.Status, condition.Reason, condition.Message)
		}
		return false, "not done waiting for APIExportIdentity to be marked valid, no condition exists"
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "could not wait for APIExport to be valid with identity hash")
	require.NotNil(t, sheriffExport)

	t.Logf("creating consumer namespace")
	consumerNS1, err := kubeClusterClient.Cluster(consumerPath).CoreV1().Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "consumer-ns-1",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create ns-1")

	t.Logf("waiting for consumer namespace to exist")
	framework.Eventually(t, func() (done bool, str string) {
		consumerNS1, err := kubeClusterClient.Cluster(consumerPath).CoreV1().Namespaces().Get(ctx, consumerNS1.Name, metav1.GetOptions{})
		if err != nil {
			return false, err.Error()
		}

		if consumerNS1.Status.Phase == v1.NamespaceActive {
			return true, ""
		}

		return false, "not done waiting for ns1 to be created"
	}, wait.ForeverTestTimeout, 110*time.Millisecond, "could not wait for namespace to be ready")
	t.Logf("namespace %s ready", consumerNS1.Name)

	t.Logf("setting PermissionClaims on APIExport %s", sheriffExport.Name)
	sheriffExport.Spec.PermissionClaims = makeNarrowCMPermissionClaims("*", "consumer-ns-1")
	framework.Eventually(t, func() (done bool, str string) {
		updatedSheriffExport, err := kcpClusterClient.Cluster(serviceProviderPath).ApisV1alpha1().APIExports().Update(ctx, sheriffExport, metav1.UpdateOptions{})
		if err != nil {
			return false, err.Error()
		}

		sheriffExport = updatedSheriffExport
		return true, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "could not wait for APIExport to be updated with PermissionClaims")

	t.Logf("binding consumer cluster and namespace to provider export")
	binding := bindConsumerToProviderCMExport(ctx, t, consumerPath, serviceProviderPath, kcpClusterClient, "*", consumerNS1.Name)
	require.NotNil(t, binding)

	apiExportVWCfg := rest.CopyConfig(cfg)
	//nolint:staticcheck // SA1019 VirtualWorkspaces is deprecated but not removed yet
	apiExportVWCfg.Host = sheriffExport.Status.VirtualWorkspaces[0].URL
	t.Logf("vwHost: %s", apiExportVWCfg.Host)
	apiExportClient, err := kcpkubernetesclientset.NewForConfig(apiExportVWCfg)
	require.NoError(t, err)

	t.Logf("verify we can create a new configmap in consumer namespace via the sheriff export view URL")
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "confmap1",
		},
	}
	newCM := &v1.ConfigMap{}
	framework.Eventually(t, func() (done bool, str string) {
		newCM, err = apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS1.Name).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return false, err.Error()
		}

		return true, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "timed out trying to create configmap in consumer namespace")
	require.Equal(t, consumerNS1.Name, newCM.Namespace)
	t.Logf("cluster for %s: %s", newCM.Name, logicalcluster.From(newCM).String())

	t.Logf("verify we can update a configmap in consumer workspace via the view URL")
	cm.Namespace = consumerNS1.Name
	cm.Data = map[string]string{
		"something": "new",
	}
	times := 0
	updatedCM := &v1.ConfigMap{}
	framework.Eventually(t, func() (done bool, str string) {
		times = times + 1
		updatedCM, err = apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS1.Name).Update(ctx, cm, metav1.UpdateOptions{})
		if err != nil {
			return false, err.Error()
		}
		return true, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "timed out trying to update configmap in consumer namespace %s, %v", consumerNS1.Name, cm)
	require.NotNil(t, updatedCM.Data)
	require.Equal(t, "new", updatedCM.Data["something"])

	t.Logf("ensure that configmaps in an unspecified namespace cannot be created via view URL")
	t.Logf("creating unclaimed consumer namespace")
	consumerNS2, err := kubeClusterClient.Cluster(consumerPath).CoreV1().Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "consumer-ns-2",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create %s", consumerNS2.Name)

	t.Logf("waiting for namespace %s to exist", consumerNS2.Name)
	framework.Eventually(t, func() (done bool, str string) {
		consumerNS1, err := kubeClusterClient.Cluster(consumerPath).CoreV1().Namespaces().Get(ctx, consumerNS2.Name, metav1.GetOptions{})
		if err != nil {
			return false, err.Error()
		}

		if consumerNS1.Status.Phase == v1.NamespaceActive {
			return true, ""
		}

		return false, "not done waiting for ns2 to be created"
	}, wait.ForeverTestTimeout, 110*time.Millisecond, "could not wait for namespace to be ready")
	t.Logf("namespace %s ready", consumerNS2.Name)

	// clear namespace so we can submit to a different one
	cm.Namespace = ""
	framework.Eventually(t, func() (done bool, str string) {
		newCM, err = apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS2.Name).Create(ctx, cm, metav1.CreateOptions{})
		if apierrors.IsForbidden(err) {
			return true, ""
		}
		return false, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "timed out trying to create configmap in consumer namespace")
	t.Logf("creation of configmap in namespace %s successfully forbidden", consumerNS2.Name)

	t.Logf("updating permission claims to allow configmap by explicit name in any namespace")
	t.Logf("setting PermissionClaims on APIExport %s", sheriffExport.Name)
	sheriffExport.Spec.PermissionClaims = makeNarrowCMPermissionClaims("confmap1", "*")
	framework.Eventually(t, func() (done bool, str string) {
		sheriffExport, err = kcpClusterClient.Cluster(serviceProviderPath).ApisV1alpha1().APIExports().Update(ctx, sheriffExport, metav1.UpdateOptions{})
		if err != nil {
			return false, err.Error()
		}

		return true, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "could not wait for APIExport to be updated with PermissionClaims")

	t.Logf("updating consumer API Bindings with new permissionclaim")
	binding = bindConsumerToProviderCMExport(ctx, t, consumerPath, serviceProviderPath, kcpClusterClient, "confmap1", "*")
	require.NotNil(t, binding)

	cm = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "confmap1",
		},
	}
	t.Logf("creating configmap %s in NS %s", cm.Name, consumerNS2.Name)
	framework.Eventually(t, func() (done bool, str string) {
		cm, err = apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS2.Name).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return false, err.Error()
		}

		return true, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "timed out trying to create configmap in consumer namespace")
	require.Equal(t, consumerNS2.Name, cm.Namespace)
	require.NoError(t, err)
	t.Logf("cluster for configmap %s: %s", cm.Name, logicalcluster.From(cm).String())

	framework.Eventually(t, func() (done bool, str string) {
		updatedCM, err := apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS2.Name).Get(ctx, cm.Name, metav1.GetOptions{})
		if err != nil {
			return false, err.Error()
		}
		require.NoError(t, err)
		require.Equal(t, consumerNS2.Name, updatedCM.Namespace)

		return true, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "timed out trying to get configmap in %s", consumerNS2.Name)

	t.Logf("verify we can update a configmap in consumer workspace via the view URL")
	cm.Data = map[string]string{
		"something": "new",
	}
	updateCM := func() (bool, string) {
		var newCM *v1.ConfigMap
		newCM, err = apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS2.Name).Update(ctx, cm, metav1.UpdateOptions{})
		if apierrors.IsNotFound(err) {
			t.Logf("couldn't find configmap in %s", consumerNS2.Name)
		}
		if err != nil {
			return false, err.Error()
		}
		cm = newCM
		return true, ""
	}
	framework.Eventually(t, updateCM, wait.ForeverTestTimeout, 100*time.Millisecond, "timed out trying to update configmap in %s", consumerNS2.Name)
	require.Equal(t, cm.Data["something"], "new")

	t.Logf("update PermissionClaims to only work in one namespace")
	t.Logf("setting PermissionClaims on APIExport %s", sheriffExport.Name)
	sheriffExport.Spec.PermissionClaims = makeNarrowCMPermissionClaims("", consumerNS1.Name)
	framework.Eventually(t, func() (done bool, str string) {
		sheriffExport, err = kcpClusterClient.Cluster(serviceProviderPath).ApisV1alpha1().APIExports().Update(ctx, sheriffExport, metav1.UpdateOptions{})
		if err != nil {
			return false, err.Error()
		}

		return true, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "could not wait for APIExport to be updated with PermissionClaims")

	t.Logf("updating consumer APIBindings with single namespace PermissionClaim")
	binding = bindConsumerToProviderCMExport(ctx, t, consumerPath, serviceProviderPath, kcpClusterClient, "*", consumerNS1.Name)
	require.NotNil(t, binding)
	cm = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "confmap2",
		},
	}
	framework.Eventually(t, func() (done bool, str string) {
		cm, err = apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS2.Name).Create(ctx, cm, metav1.CreateOptions{})
		if apierrors.IsForbidden(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}

		return false, "unexpected create"
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "never received forbidden error")

	t.Logf("update PermissionClaims to only allow a specific object name in a specific namespace")
	t.Logf("setting PermissionClaims on APIExport %s", sheriffExport.Name)
	sheriffExport.Spec.PermissionClaims = makeNarrowCMPermissionClaims("unique", consumerNS1.Name)
	framework.Eventually(t, func() (done bool, str string) {
		sheriffExport, err = kcpClusterClient.Cluster(serviceProviderPath).ApisV1alpha1().APIExports().Update(ctx, sheriffExport, metav1.UpdateOptions{})
		if err != nil {
			return false, err.Error()
		}

		return true, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "could not wait for APIExport to be updated with PermissionClaims")

	t.Logf("updating consumer APIBindings with single name/namespace PermissionClaim")
	binding = bindConsumerToProviderCMExport(ctx, t, consumerPath, serviceProviderPath, kcpClusterClient, "unique", consumerNS1.Name)
	require.NotNil(t, binding)
	cm = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unique",
		},
	}
	t.Logf("confirm named configmap can be created")
	framework.Eventually(t, func() (done bool, str string) {
		cm, err = apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS1.Name).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return false, err.Error()
		}

		return true, ""
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "timed out trying to create configmap in %s", consumerNS1.Name)
	require.Equal(t, "consumer-ns-1", cm.Namespace)
	require.NoError(t, err)

	t.Logf("confirm configmaps with unpermitted names cannot be created")
	badCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "not-unique",
		},
	}
	framework.Eventually(t, func() (done bool, str string) {
		_, err = apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS1.Name).Create(ctx, badCM, metav1.CreateOptions{})
		if apierrors.IsForbidden(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}

		return false, "unexpected create"
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "never received forbidden error")

	t.Logf("create a configmap that does not match permision claims in consumer namespace, outside the view")
	framework.Eventually(t, func() (done bool, str string) {
		_, err := kubeClusterClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS1.Name).Create(ctx, badCM, metav1.CreateOptions{})
		if err != nil {
			return false, err.Error()
		}

		return true, "created configmap outside view url"

	}, wait.ForeverTestTimeout, 100*time.Millisecond, "could not create configmap outside view url")

	t.Logf("listing configmaps through view URL only returns applicable objects")
	framework.Eventually(t, func() (done bool, str string) {
		list, err := apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS1.Name).List(ctx, metav1.ListOptions{})
		require.Equal(t, 1, len(list.Items))
		require.Equal(t, "unique", list.Items[0].Name)
		if err != nil {
			return false, err.Error()
		}

		return true, "got expected items in list"

	}, wait.ForeverTestTimeout, 100*time.Millisecond, "could not list configmaps")

	t.Logf("deleting claimed configmaps through the view url")
	framework.Eventually(t, func() (done bool, str string) {
		err := apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS1.Name).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
		if err != nil {
			return false, err.Error()
		}

		return true, "successfully deleted configmaps through view url"

	}, wait.ForeverTestTimeout, 100*time.Millisecond, "timed out waiting to delete configmaps")

	t.Logf("getting configmaps that were not covered by permission claims")
	framework.Eventually(t, func() (done bool, str string) {
		list, err := kubeClusterClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS1.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err.Error()
		}

		require.Equal(t, 3, len(list.Items))
		names := make([]string, 0, 3)
		for _, i := range list.Items {
			names = append(names, i.Name)
		}
		require.ElementsMatch(t, names, []string{"not-unique", "kube-root-ca.crt", "confmap1"})

		return true, "got expected items in list"

	}, wait.ForeverTestTimeout, 100*time.Millisecond, "could not list configmaps")

	t.Logf("trying to delete single configmap not covered by permission claims")
	framework.Eventually(t, func() (done bool, str string) {
		err := apiExportClient.Cluster(consumerPath).CoreV1().ConfigMaps(consumerNS1.Name).Delete(ctx, "confmap1", metav1.DeleteOptions{})
		if apierrors.IsForbidden(err) {
			return true, ""
		}

		if err != nil {
			return false, err.Error()
		}

		return false, "delete unexpectedly successful"

	}, wait.ForeverTestTimeout, 100*time.Millisecond, "could not delete configmap")
}

// makeNarrowCMPermissionClaim creates a PermissionClaim for ConfigMaps scoped to just a name, just a namespace, or both.
func makeNarrowCMPermissionClaims(name, namespace string) []apisv1alpha1.PermissionClaim {
	return []apisv1alpha1.PermissionClaim{
		{
			GroupResource: apisv1alpha1.GroupResource{Group: "", Resource: "configmaps"},
			All:           false,
			ResourceSelector: []apisv1alpha1.ResourceSelector{
				{
					Names:      []string{name},
					Namespaces: []string{namespace},
				},
			},
		},
	}
}

func makeAcceptedCMPermissionClaims(name, namespace string) []apisv1alpha1.AcceptablePermissionClaim {
	return []apisv1alpha1.AcceptablePermissionClaim{
		{
			PermissionClaim: apisv1alpha1.PermissionClaim{
				GroupResource: apisv1alpha1.GroupResource{Group: "", Resource: "configmaps"},
				ResourceSelector: []apisv1alpha1.ResourceSelector{
					{
						Names:      []string{name},
						Namespaces: []string{namespace},
					},
				},
				All: false,
			},
			State: apisv1alpha1.ClaimAccepted,
		},
	}
}
func bindConsumerToProviderCMExport(
	ctx context.Context,
	t *testing.T,
	consumerPath logicalcluster.Path,
	providerClusterPath logicalcluster.Path,
	kcpClusterClients kcpclientset.ClusterInterface,
	cmName, cmNamespace string,
) *apisv1alpha1.APIBinding {
	t.Helper()
	t.Logf("create an APIBinding in consumer workspace %q that points to the today-cowboys export from %q", consumerPath, providerClusterPath)
	apiBinding := &apisv1alpha1.APIBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sheriffs-and-configmaps",
		},
		Spec: apisv1alpha1.APIBindingSpec{
			Reference: apisv1alpha1.BindingReference{
				Export: &apisv1alpha1.ExportBindingReference{
					Path: providerClusterPath.String(),
					Name: "wild.wild.west",
				},
			},
			PermissionClaims: makeAcceptedCMPermissionClaims(cmName, cmNamespace),
		},
	}

	binding := &apisv1alpha1.APIBinding{}
	framework.Eventually(t, func() (bool, string) {
		var err error
		binding, err = kcpClusterClients.Cluster(consumerPath).ApisV1alpha1().APIBindings().Create(ctx, apiBinding, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			binding, err = kcpClusterClients.Cluster(consumerPath).ApisV1alpha1().APIBindings().Get(ctx, apiBinding.Name, metav1.GetOptions{})
			require.NoError(t, err)
			binding.Spec.PermissionClaims = makeAcceptedCMPermissionClaims(cmName, cmNamespace)
			t.Logf("resource version on the APIBinding: %s", binding.ResourceVersion)
			binding, err = kcpClusterClients.Cluster(consumerPath).ApisV1alpha1().APIBindings().Update(ctx, binding, metav1.UpdateOptions{})
			require.NoError(t, err)
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return true, ""
	}, wait.ForeverTestTimeout, time.Millisecond*100)

	return binding
}
