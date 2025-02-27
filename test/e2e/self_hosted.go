/*
Copyright 2020 The Kubernetes Authors.

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

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/test/e2e/internal/log"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	"sigs.k8s.io/cluster-api/util"
)

// SelfHostedSpecInput is the input for SelfHostedSpec.
type SelfHostedSpecInput struct {
	E2EConfig             *clusterctl.E2EConfig
	ClusterctlConfigPath  string
	BootstrapClusterProxy framework.ClusterProxy
	ArtifactFolder        string
	SkipCleanup           bool
	ControlPlaneWaiters   clusterctl.ControlPlaneWaiters
	Flavor                string
}

// SelfHostedSpec implements a test that verifies Cluster API creating a cluster, pivoting to a self-hosted cluster.
// NOTE: This test works with Clusters with and without ClusterClass.
func SelfHostedSpec(ctx context.Context, inputGetter func() SelfHostedSpecInput) {
	var (
		specName         = "self-hosted"
		input            SelfHostedSpecInput
		namespace        *corev1.Namespace
		cancelWatches    context.CancelFunc
		clusterResources *clusterctl.ApplyClusterTemplateAndWaitResult

		selfHostedClusterProxy  framework.ClusterProxy
		selfHostedNamespace     *corev1.Namespace
		selfHostedCancelWatches context.CancelFunc
		selfHostedCluster       *clusterv1.Cluster
	)

	BeforeEach(func() {
		Expect(ctx).NotTo(BeNil(), "ctx is required for %s spec", specName)
		input = inputGetter()
		Expect(input.E2EConfig).ToNot(BeNil(), "Invalid argument. input.E2EConfig can't be nil when calling %s spec", specName)
		Expect(input.ClusterctlConfigPath).To(BeAnExistingFile(), "Invalid argument. input.ClusterctlConfigPath must be an existing file when calling %s spec", specName)
		Expect(input.BootstrapClusterProxy).ToNot(BeNil(), "Invalid argument. input.BootstrapClusterProxy can't be nil when calling %s spec", specName)
		Expect(os.MkdirAll(input.ArtifactFolder, 0750)).To(Succeed(), "Invalid argument. input.ArtifactFolder can't be created for %s spec", specName)
		Expect(input.E2EConfig.Variables).To(HaveKey(KubernetesVersion))

		// Setup a Namespace where to host objects for this spec and create a watcher for the namespace events.
		namespace, cancelWatches = setupSpecNamespace(ctx, specName, input.BootstrapClusterProxy, input.ArtifactFolder)
		clusterResources = new(clusterctl.ApplyClusterTemplateAndWaitResult)
	})

	It("Should pivot the bootstrap cluster to a self-hosted cluster", func() {
		By("Creating a workload cluster")

		workloadClusterName := fmt.Sprintf("%s-%s", specName, util.RandomString(6))
		clusterctl.ApplyClusterTemplateAndWait(ctx, clusterctl.ApplyClusterTemplateAndWaitInput{
			ClusterProxy: input.BootstrapClusterProxy,
			ConfigCluster: clusterctl.ConfigClusterInput{
				LogFolder:                filepath.Join(input.ArtifactFolder, "clusters", input.BootstrapClusterProxy.GetName()),
				ClusterctlConfigPath:     input.ClusterctlConfigPath,
				KubeconfigPath:           input.BootstrapClusterProxy.GetKubeconfigPath(),
				InfrastructureProvider:   clusterctl.DefaultInfrastructureProvider,
				Flavor:                   input.Flavor,
				Namespace:                namespace.Name,
				ClusterName:              workloadClusterName,
				KubernetesVersion:        input.E2EConfig.GetVariable(KubernetesVersion),
				ControlPlaneMachineCount: pointer.Int64Ptr(1),
				WorkerMachineCount:       pointer.Int64Ptr(1),
			},
			ControlPlaneWaiters:          input.ControlPlaneWaiters,
			WaitForClusterIntervals:      input.E2EConfig.GetIntervals(specName, "wait-cluster"),
			WaitForControlPlaneIntervals: input.E2EConfig.GetIntervals(specName, "wait-control-plane"),
			WaitForMachineDeployments:    input.E2EConfig.GetIntervals(specName, "wait-worker-nodes"),
		}, clusterResources)

		By("Turning the workload cluster into a management cluster")

		// In case the cluster is a DockerCluster, we should load controller images into the nodes.
		// Nb. this can be achieved also by changing the DockerMachine spec, but for the time being we are using
		// this approach because this allows to have a single source of truth for images, the e2e config
		// Nb. If the cluster is a managed topology cluster.Spec.InfrastructureRef will be nil till
		// the cluster object is reconciled. Therefore, we always try to fetch the reconciled cluster object from
		// the server to check if it is a DockerCluster.
		cluster := clusterResources.Cluster
		isDockerCluster := false
		Eventually(func() error {
			c := input.BootstrapClusterProxy.GetClient()
			tmpCluster := &clusterv1.Cluster{}
			if err := c.Get(ctx, client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}, tmpCluster); err != nil {
				return err
			}
			if tmpCluster.Spec.InfrastructureRef != nil {
				isDockerCluster = tmpCluster.Spec.InfrastructureRef.Kind == "DockerCluster"
				return nil
			}
			return errors.New("cluster object not yet reconciled")
		}, "1m", "5s").Should(Succeed(), "Failed to get the Cluster %s", klog.KObj(cluster))

		if isDockerCluster {
			Expect(bootstrap.LoadImagesToKindCluster(ctx, bootstrap.LoadImagesToKindClusterInput{
				Name:   cluster.Name,
				Images: input.E2EConfig.Images,
			})).To(Succeed())
		}

		// Get a ClusterBroker so we can interact with the workload cluster
		selfHostedClusterProxy = input.BootstrapClusterProxy.GetWorkloadCluster(ctx, cluster.Namespace, cluster.Name)

		Byf("Creating a namespace for hosting the %s test spec", specName)
		selfHostedNamespace, selfHostedCancelWatches = framework.CreateNamespaceAndWatchEvents(ctx, framework.CreateNamespaceAndWatchEventsInput{
			Creator:   selfHostedClusterProxy.GetClient(),
			ClientSet: selfHostedClusterProxy.GetClientSet(),
			Name:      namespace.Name,
			LogFolder: filepath.Join(input.ArtifactFolder, "clusters", "bootstrap"),
		})

		By("Initializing the workload cluster")
		clusterctl.InitManagementClusterAndWatchControllerLogs(ctx, clusterctl.InitManagementClusterAndWatchControllerLogsInput{
			ClusterProxy:              selfHostedClusterProxy,
			ClusterctlConfigPath:      input.ClusterctlConfigPath,
			InfrastructureProviders:   input.E2EConfig.InfrastructureProviders(),
			IPAMProviders:             input.E2EConfig.IPAMProviders(),
			RuntimeExtensionProviders: input.E2EConfig.RuntimeExtensionProviders(),
			LogFolder:                 filepath.Join(input.ArtifactFolder, "clusters", cluster.Name),
		}, input.E2EConfig.GetIntervals(specName, "wait-controllers")...)

		By("Ensure API servers are stable before doing move")
		// Nb. This check was introduced to prevent doing move to self-hosted in an aggressive way and thus avoid flakes.
		// More specifically, we were observing the test failing to get objects from the API server during move, so we
		// are now testing the API servers are stable before starting move.
		Consistently(func() error {
			kubeSystem := &corev1.Namespace{}
			return input.BootstrapClusterProxy.GetClient().Get(ctx, client.ObjectKey{Name: "kube-system"}, kubeSystem)
		}, "5s", "100ms").Should(BeNil(), "Failed to assert bootstrap API server stability")
		Consistently(func() error {
			kubeSystem := &corev1.Namespace{}
			return selfHostedClusterProxy.GetClient().Get(ctx, client.ObjectKey{Name: "kube-system"}, kubeSystem)
		}, "5s", "100ms").Should(BeNil(), "Failed to assert self-hosted API server stability")

		// Get the machines of the workloadCluster before it is moved to become self-hosted to make sure that the move did not trigger
		// any unexpected rollouts.
		preMoveMachineList := &unstructured.UnstructuredList{}
		preMoveMachineList.SetGroupVersionKind(clusterv1.GroupVersion.WithKind("MachineList"))
		err := input.BootstrapClusterProxy.GetClient().List(
			ctx,
			preMoveMachineList,
			client.InNamespace(namespace.Name),
			client.MatchingLabels{clusterv1.ClusterLabelName: workloadClusterName},
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to list machines before move")

		By("Moving the cluster to self hosted")
		clusterctl.Move(ctx, clusterctl.MoveInput{
			LogFolder:            filepath.Join(input.ArtifactFolder, "clusters", "bootstrap"),
			ClusterctlConfigPath: input.ClusterctlConfigPath,
			FromKubeconfigPath:   input.BootstrapClusterProxy.GetKubeconfigPath(),
			ToKubeconfigPath:     selfHostedClusterProxy.GetKubeconfigPath(),
			Namespace:            namespace.Name,
		})

		log.Logf("Waiting for the cluster to be reconciled after moving to self hosted")
		selfHostedCluster = framework.DiscoveryAndWaitForCluster(ctx, framework.DiscoveryAndWaitForClusterInput{
			Getter:    selfHostedClusterProxy.GetClient(),
			Namespace: selfHostedNamespace.Name,
			Name:      cluster.Name,
		}, input.E2EConfig.GetIntervals(specName, "wait-cluster")...)

		controlPlane := framework.GetKubeadmControlPlaneByCluster(ctx, framework.GetKubeadmControlPlaneByClusterInput{
			Lister:      selfHostedClusterProxy.GetClient(),
			ClusterName: selfHostedCluster.Name,
			Namespace:   selfHostedCluster.Namespace,
		})
		Expect(controlPlane).ToNot(BeNil())

		// After the move check that there were no unexpected rollouts.
		Consistently(func() bool {
			postMoveMachineList := &unstructured.UnstructuredList{}
			postMoveMachineList.SetGroupVersionKind(clusterv1.GroupVersion.WithKind("MachineList"))
			err = selfHostedClusterProxy.GetClient().List(
				ctx,
				postMoveMachineList,
				client.InNamespace(namespace.Name),
				client.MatchingLabels{clusterv1.ClusterLabelName: workloadClusterName},
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to list machines after move")
			return matchUnstructuredLists(preMoveMachineList, postMoveMachineList)
		}, "3m", "30s").Should(BeTrue(), "Machines should not roll out after move to self-hosted cluster")

		By("PASSED!")
	})

	AfterEach(func() {
		if selfHostedNamespace != nil {
			// Dump all Cluster API related resources to artifacts before pivoting back.
			framework.DumpAllResources(ctx, framework.DumpAllResourcesInput{
				Lister:    selfHostedClusterProxy.GetClient(),
				Namespace: namespace.Name,
				LogPath:   filepath.Join(input.ArtifactFolder, "clusters", clusterResources.Cluster.Name, "resources"),
			})
		}
		if selfHostedCluster != nil {
			By("Ensure API servers are stable before doing move")
			// Nb. This check was introduced to prevent doing move back to bootstrap in an aggressive way and thus avoid flakes.
			// More specifically, we were observing the test failing to get objects from the API server during move, so we
			// are now testing the API servers are stable before starting move.
			Consistently(func() error {
				kubeSystem := &corev1.Namespace{}
				return input.BootstrapClusterProxy.GetClient().Get(ctx, client.ObjectKey{Name: "kube-system"}, kubeSystem)
			}, "5s", "100ms").Should(BeNil(), "Failed to assert bootstrap API server stability")
			Consistently(func() error {
				kubeSystem := &corev1.Namespace{}
				return selfHostedClusterProxy.GetClient().Get(ctx, client.ObjectKey{Name: "kube-system"}, kubeSystem)
			}, "5s", "100ms").Should(BeNil(), "Failed to assert self-hosted API server stability")

			By("Moving the cluster back to bootstrap")
			clusterctl.Move(ctx, clusterctl.MoveInput{
				LogFolder:            filepath.Join(input.ArtifactFolder, "clusters", clusterResources.Cluster.Name),
				ClusterctlConfigPath: input.ClusterctlConfigPath,
				FromKubeconfigPath:   selfHostedClusterProxy.GetKubeconfigPath(),
				ToKubeconfigPath:     input.BootstrapClusterProxy.GetKubeconfigPath(),
				Namespace:            selfHostedNamespace.Name,
			})

			log.Logf("Waiting for the cluster to be reconciled after moving back to bootstrap")
			clusterResources.Cluster = framework.DiscoveryAndWaitForCluster(ctx, framework.DiscoveryAndWaitForClusterInput{
				Getter:    input.BootstrapClusterProxy.GetClient(),
				Namespace: namespace.Name,
				Name:      clusterResources.Cluster.Name,
			}, input.E2EConfig.GetIntervals(specName, "wait-cluster")...)
		}
		if selfHostedCancelWatches != nil {
			selfHostedCancelWatches()
		}

		// Dumps all the resources in the spec namespace, then cleanups the cluster object and the spec namespace itself.
		dumpSpecResourcesAndCleanup(ctx, specName, input.BootstrapClusterProxy, input.ArtifactFolder, namespace, cancelWatches, clusterResources.Cluster, input.E2EConfig.GetIntervals, input.SkipCleanup)
	})
}
