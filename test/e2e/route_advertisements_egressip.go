// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

const (
	egressBGPExportLabelKey   = "egress-nodes"
	egressBGPExportLabelValue = "bgp"

	bgpEgressIPClientPodLabelKey   = "bgp-eip-client"
	bgpEgressIPClientPodLabelValue = "enabled"

	bgpEgressIPLocalPodName  = "bgp-eip-client-local"
	bgpEgressIPRemotePodName = "bgp-eip-client-remote"

	bgpEgressIPPollTimeout                = 3 * retryTimeout
	bgpEgressIPGRPCHealthCheckPort        = "9107"
	bgpEgressIPPodHTTPPort         uint16 = 8080

	// Matches FRRConfiguration created by contrib/kind-common.sh and the default-network RA from helm when advertiseDefaultNetwork is enabled.
	bgpE2EFRRConfigurationLabelKey   = "name"
	bgpE2EFRRConfigurationLabelValue = "receive-all"
)

type bgpEgressIPNodeSet struct {
	remote  corev1.Node
	egressA corev1.Node
	egressB corev1.Node
}

type bgpEgressIPScenario struct {
	// True: EgressIP RouteAdvertisements uses nodeSelector egress-nodes=bgp (cases 2 and 5 in this suite).
	// False: empty nodeSelector — any egress-assignable node holding an address may advertise (cases 1 and 4).
	useRouteAdvertisementsNodeSelector bool

	// Only when useRouteAdvertisementsNodeSelector is true:
	// True (case 2): placement on RA-matching egress workers; failover one address from a matching worker to
	// remote (egress-assignable only, not a selector match).
	// False (case 5): placement on non-matching egress workers; failover one address from a non-matching worker to
	// remote (egress-assignable + RA match label on remote).
	failoverFromAdvertised bool
}

var _ = ginkgo.Describe("BGP: EgressIP advertisement", feature.RouteAdvertisements, feature.EgressIP, ginkgo.Ordered, func() {
	f := wrappedTestFramework("bgp-egressip-advertisement")

	var (
		nodeSet            bgpEgressIPNodeSet
		serverContainerIPs []string
		raClient           raclientset.Interface
	)

	ginkgo.BeforeEach(func() {
		if os.Getenv("ENABLE_ROUTE_ADVERTISEMENTS") != "true" {
			e2eskipper.Skipf("route advertisements are not enabled")
		}
		skipUnlessEnoughWorkerNodesForBGPEgressIP(f.ClientSet)

		var err error
		raClient, err = raclientset.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err)

		serverContainerIPs = getBGPServerContainerIPs(f)
		gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))

		ginkgo.By("Selecting 3 schedulable nodes for BGP egress IP (remote + 2 egress), matching egressip.go")
		nodeSet, err = pickBGPEgressIPNodeSet(f.ClientSet)
		framework.ExpectNoError(err)
		ensureOVNEgressIPFeatureEnabled(f.ClientSet)
		waitEgressNodesPrerequisitesForEgressIP(f.ClientSet, nodeSet.egressA.Name, nodeSet.egressB.Name, nodeSet.remote.Name)
		ensureBGPEgressIPHealthCheckReachable(nodeSet.egressA.Name, nodeSet.egressB.Name, nodeSet.remote.Name)
		ensureBGPEgressIPHealthCheckPortSynced(f.ClientSet)
		ensureBGPEgressIPReachabilityTimeoutForTests(f.ClientSet)
	})

	// Case 1: no RA nodeSelector — failover from an egress worker to remote (egress-assignable only).
	ginkgo.It("on default network without RouteAdvertisements nodeSelector", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, nil, bgpEgressIPScenario{
			useRouteAdvertisementsNodeSelector: false,
		})
	})

	// Case 2: RA nodeSelector — matching egress workers; failover to remote without selector match.
	ginkgo.It("on default network with RouteAdvertisements nodeSelector", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, nil, bgpEgressIPScenario{
			useRouteAdvertisementsNodeSelector: true,
			failoverFromAdvertised:             true,
		})
	})

	ginkgo.It("rejects EgressIP RouteAdvertisements on Layer2 primary CUDN", func() {
		runBGPEgressIPLayer2Rejection(f, raClient)
	})

	// Case 4: L3 CUDN, no RA nodeSelector — same failover as case 1.
	ginkgo.It("on L3 primary CUDN without RouteAdvertisements nodeSelector", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, createBGPEgressIPL3CUDN, bgpEgressIPScenario{
			useRouteAdvertisementsNodeSelector: false,
		})
	})

	// Case 5: L3 CUDN + RA nodeSelector — non-matching egress workers; failover to remote with selector match.
	ginkgo.It("on L3 primary CUDN with RouteAdvertisements nodeSelector", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, createBGPEgressIPL3CUDN, bgpEgressIPScenario{
			useRouteAdvertisementsNodeSelector: true,
			failoverFromAdvertised:             false,
		})
	})
})

func runBGPEgressIPLayer2Rejection(f *framework.Framework, raClient raclientset.Interface) {
	suffix := framework.RandomSuffix()
	cudnLabel := "bgp-eip-l2-" + suffix
	raName := "bgp-eip-l2-ra-" + suffix

	udnNamespace, err := f.CreateNamespace(context.TODO(), f.BaseName+"-l2-"+suffix, map[string]string{
		"e2e-framework":           f.BaseName,
		RequiredUDNNamespaceLabel: "",
	})
	framework.ExpectNoError(err)
	ginkgo.DeferCleanup(func() {
		_ = f.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), udnNamespace.Name, metav1.DeleteOptions{})
	})

	udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
	framework.ExpectNoError(err)

	cudn := &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bgp-eip-l2-",
			Labels:       map[string]string{cudnLabel: ""},
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{udnNamespace.Name},
			}}},
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    "Primary",
					Subnets: filterDualStackCIDRs(f.ClientSet, udnv1.DualStackCIDRs{"103.0.0.0/16", "2014:100::0/60"}),
				},
			},
		},
	}
	createdCUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudn, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	ginkgo.DeferCleanup(func() {
		_ = f.ClientSet.CoreV1().Pods(udnNamespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
		_ = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), createdCUDN.Name, metav1.DeleteOptions{})
	})
	gomega.Eventually(clusterUserDefinedNetworkReadyFunc(context.Background(), f.DynamicClient, createdCUDN.Name), 30*time.Second, time.Second).Should(gomega.Succeed())

	waitBGPClusterReadyForRouteAdvertisements()
	frrSelector := bgpE2ERouteAdvertisementFRRConfigurationSelector(raClient)

	ra := &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{Name: raName},
		Spec: rav1.RouteAdvertisementsSpec{
			NetworkSelectors: apitypes.NetworkSelectors{
				{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{cudnLabel: ""},
						},
					},
				},
			},
			NodeSelector:             metav1.LabelSelector{},
			FRRConfigurationSelector: frrSelector,
			Advertisements:           []rav1.AdvertisementType{rav1.EgressIP},
		},
	}
	_, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	ginkgo.DeferCleanup(func() {
		_ = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
	})

	waitRouteAdvertisementConfigurationError(raClient, raName, "Layer2")
}

func createBGPEgressIPL3CUDN(f *framework.Framework) (string, string, func()) {
	suffix := framework.RandomSuffix()
	cudnLabel := "bgp-eip-l3-" + suffix

	namespace, err := f.CreateNamespace(context.TODO(), f.BaseName+"-"+suffix, map[string]string{
		"e2e-framework":           f.BaseName,
		RequiredUDNNamespaceLabel: "",
	})
	framework.ExpectNoError(err)
	f.Namespace = namespace
	updateNamespaceLabels(f, namespace, map[string]string{"name": namespace.Name})

	udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
	framework.ExpectNoError(err)

	cudn := &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bgp-eip-l3-",
			Labels:       map[string]string{cudnLabel: ""},
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{f.Namespace.Name},
			}}},
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role:    "Primary",
					Subnets: filterL3Subnets(f.ClientSet, cudnLayer3SubnetsB()),
				},
			},
		},
	}
	createdCUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudn, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	gomega.Eventually(clusterUserDefinedNetworkReadyFunc(context.Background(), f.DynamicClient, createdCUDN.Name), 60*time.Second, time.Second).Should(gomega.Succeed())

	cleanup := func() {
		_ = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), createdCUDN.Name, metav1.DeleteOptions{})
	}
	return cudnLabel, util.GenerateCUDNNetworkName(createdCUDN.Name), cleanup
}

func runBGPEgressIPScenario(
	f *framework.Framework,
	raClient raclientset.Interface,
	nodeSet bgpEgressIPNodeSet,
	serverContainerIPs []string,
	setupNetwork func(*framework.Framework) (cudnMatchLabel, hostSubnetNetworkName string, cleanup func()),
	scenario bgpEgressIPScenario,
) {
	suffix := framework.RandomSuffix()
	raName := "bgp-eip-ra-" + suffix
	eipName := "bgp-eip-" + suffix
	framework.Logf("BGP egress IP node roles: remote=%s egressA=%s egressB=%s",
		nodeSet.remote.Name, nodeSet.egressA.Name, nodeSet.egressB.Name)

	var cudnMatchLabel, hostSubnetNetworkName string
	if setupNetwork != nil {
		var cleanup func()
		cudnMatchLabel, hostSubnetNetworkName, cleanup = setupNetwork(f)
		ginkgo.DeferCleanup(cleanup)
		ginkgo.DeferCleanup(func() {
			_ = f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
		})
	} else {
		updateNamespaceLabels(f, f.Namespace, map[string]string{"name": f.Namespace.Name})
	}

	ginkgo.DeferCleanup(func() {
		unlabelEgressAssignable(f.ClientSet, nodeSet.egressA.Name)
		unlabelEgressAssignable(f.ClientSet, nodeSet.egressB.Name)
	})

	if scenario.useRouteAdvertisementsNodeSelector && scenario.failoverFromAdvertised {
		// Case 2: RA nodeSelector matches on both egress workers (dual-stack needs two distinct matching nodes).
		labelBGPEgressExportOnNode(f.ClientSet, nodeSet.egressA.Name)
		labelBGPEgressExportOnNode(f.ClientSet, nodeSet.egressB.Name)
		ginkgo.DeferCleanup(func() {
			unlabelBGPEgressExportFromNode(f.ClientSet, nodeSet.egressA.Name)
			unlabelBGPEgressExportFromNode(f.ClientSet, nodeSet.egressB.Name)
		})
	}

	nodeSelector := metav1.LabelSelector{}
	if scenario.useRouteAdvertisementsNodeSelector {
		nodeSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{egressBGPExportLabelKey: egressBGPExportLabelValue},
		}
	}

	waitBGPClusterReadyForRouteAdvertisements()
	frrSelector := bgpE2ERouteAdvertisementFRRConfigurationSelector(raClient)

	onPrimaryCUDN := cudnMatchLabel != ""
	if onPrimaryCUDN {
		podNetRAName := "bgp-eip-udn-podnet-ra-" + suffix
		createBGPL3CUDNPodNetworkRouteAdvertisement(raClient, podNetRAName, cudnMatchLabel, frrSelector)
		ginkgo.DeferCleanup(func() {
			_ = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), podNetRAName, metav1.DeleteOptions{})
		})
		waitRouteAdvertisementAccepted(raClient, podNetRAName)
	}

	createBGPEgressIPRouteAdvertisement(raClient, raName, cudnMatchLabel, nodeSelector, frrSelector)
	ginkgo.DeferCleanup(func() {
		_ = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
	})
	waitRouteAdvertisementAccepted(raClient, raName)

	if onPrimaryCUDN {
		waitBGPEgressCUDNHostSubnetsOnNodes(f.ClientSet, hostSubnetNetworkName, nodeSet.egressA.Name, nodeSet.egressB.Name)
	}
	waitEgressNodesPrerequisitesForEgressIP(f.ClientSet, nodeSet.egressA.Name, nodeSet.egressB.Name)

	// Cluster-manager placement uses util.GetEgressIPNetwork (node-primary-ifaddr / host-cidrs), not the
	// CUDN entry in node-subnets. L3 primary CUDN pods still use machine-network egress IPs like default network.
	var egressIPs []string
	var dualPlan bgpDualStackEgressIPPlan
	egressIPs, dualPlan = allocateBGPEgressTestIPsFromMachineNetwork(f.ClientSet, nodeSet, scenario)
	framework.Logf("BGP egress IP test using %d egress address(es): %v", len(egressIPs), egressIPs)

	dualStackEIP := len(egressIPs) > 1
	// Label immediately before creating the EgressIP so cluster-manager does not run periodic
	// reachability checks on labeled nodes during the (potentially slow) address allocation loop.
	if dualStackEIP {
		prepareBGPEgressNodesForEgressIP(f.ClientSet, dualPlan.labelAtCreate...)
	} else {
		prepareBGPEgressNodesForEgressIP(f.ClientSet, nodeSet.egressA.Name, nodeSet.egressB.Name)
	}

	podLabels := map[string]string{bgpEgressIPClientPodLabelKey: bgpEgressIPClientPodLabelValue}
	nsLabels := map[string]string{"name": f.Namespace.Name}
	applyEgressIPFromManifest(createEIPManifest(eipName, podLabels, nsLabels, egressIPs...))
	ginkgo.DeferCleanup(func() {
		_, _ = e2ekubectl.RunKubectl("default", "delete", "eip", eipName, "--ignore-not-found")
	})
	triggerEgressIPObjectReconcile(eipName)
	if dualStackEIP {
		refreshEgressAssignableNodeLabels(f.ClientSet, dualPlan.labelAtCreate...)
	} else {
		refreshEgressAssignableNodeLabels(f.ClientSet, nodeSet.egressA.Name, nodeSet.egressB.Name)
	}
	assignments := waitForEgressIPAssignments(f.ClientSet, nodeSet, eipName, egressIPs, false)
	if dualStackEIP {
		v4, v6 := dualStackBGPEgressIPv4AndIPv6(egressIPs)
		waitDualStackBGPEgressIPPlacement(f.ClientSet, eipName, v4, v6, dualPlan)
		assignments = egressIPAssignmentsByAddress(getEgressIPStatusItems(eipName))
	} else {
		assertBGPEgressIPAssignmentsOnDistinctEgressNodes(assignments, nodeSet.egressA.Name, nodeSet.egressB.Name)
	}

	for egressIP, nodeName := range assignments {
		node := nodeByName(nodesFromSet(nodeSet), nodeName)
		expectExported := !scenario.useRouteAdvertisementsNodeSelector || nodeHasEgressBGPExportLabel(node)
		if expectExported {
			expectEgressIPHostRouteInFRR(node, egressIP)
		} else {
			expectEgressIPNotExportedInFRR(egressIP)
		}
	}

	localPod, err := createGenericPodWithLabel(f, bgpEgressIPLocalPodName, nodeSet.egressA.Name, f.Namespace.Name,
		getAgnHostHTTPPortBindFullCMD(bgpEgressIPPodHTTPPort), podLabels)
	framework.ExpectNoError(err)
	remotePod, err := createGenericPodWithLabel(f, bgpEgressIPRemotePodName, nodeSet.remote.Name, f.Namespace.Name,
		getAgnHostHTTPPortBindFullCMD(bgpEgressIPPodHTTPPort), podLabels)
	framework.ExpectNoError(err)
	waitForPodsReady(f.ClientSet, f.Namespace.Name, localPod.Name, remotePod.Name)

	assertEgressIPDatapathFromLocalAndRemotePods(f.Namespace.Name, localPod.Name, remotePod.Name, serverContainerIPs, assignments, onPrimaryCUDN)

	var failoverIP, failoverNode, failoverToNode string
	failoverToNode = nodeSet.remote.Name
	raMatchOnRemote := false
	if dualStackEIP {
		raMatchOnRemote = dualPlan.failoverRemoteMatchesRASelector
	} else {
		raMatchOnRemote = scenario.useRouteAdvertisementsNodeSelector && !scenario.failoverFromAdvertised
	}
	prepareBGPEgressIPFailoverRemote(f, failoverToNode, raMatchOnRemote)
	waitForClusterManagerEgressFailoverTarget(f, eipName, failoverToNode)
	if dualStackEIP {
		if dualPlan.placementOnRASelectorMatch != nil {
			failoverIP, failoverNode = pickDualStackFailoverForRAPlacement(f.ClientSet, assignments, nodeSet, dualPlan.failoverFromAdvertised, failoverToNode)
		} else {
			failoverIP, failoverNode = pickDualStackFailoverFromEgressWorkers(f.ClientSet, assignments, nodeSet, failoverToNode)
		}
	} else {
		failoverIP, failoverNode = pickFailoverTarget(f.ClientSet, assignments, nodeSet, scenario, failoverToNode)
	}
	ginkgo.By(fmt.Sprintf("Failover: moving %s from %s to %s", failoverIP, failoverNode, failoverToNode))
	otherAssignments := copyAssignmentMap(assignments)
	ginkgo.By(fmt.Sprintf("Failing over egress IP %s from node %s to node %s by removing egress-assignable", failoverIP, failoverNode, failoverToNode))
	egressAvailability := &egressNodeAvailabilityHandlerViaLabel{F: f}
	egressAvailability.Disable(failoverNode)
	ginkgo.DeferCleanup(func() { egressAvailability.Enable(failoverNode) })

	newAssignments := waitForEgressIPFailover(f.ClientSet, nodeSet, eipName, failoverIP, failoverNode, failoverToNode, otherAssignments, len(egressIPs))
	newNode := newAssignments[failoverIP]
	newNodeObj := nodeByName(nodesFromSet(nodeSet), newNode)

	if scenario.useRouteAdvertisementsNodeSelector {
		if nodeHasEgressBGPExportLabel(newNodeObj) {
			expectEgressIPHostRouteInFRR(newNodeObj, failoverIP)
		} else {
			expectEgressIPNotExportedInFRR(failoverIP)
		}
	} else {
		expectEgressIPHostRouteInFRR(newNodeObj, failoverIP)
	}

	assertEgressIPDatapathFromLocalAndRemotePods(f.Namespace.Name, localPod.Name, remotePod.Name, serverContainerIPs, map[string]string{failoverIP: newNode}, onPrimaryCUDN)
	for ip, node := range otherAssignments {
		if ip == failoverIP {
			continue
		}
		gomega.Expect(newAssignments[ip]).To(gomega.Equal(node), "unchanged egress IP %s should stay on %s", ip, node)
		nodeObj := nodeByName(nodesFromSet(nodeSet), node)
		expectExported := !scenario.useRouteAdvertisementsNodeSelector || nodeHasEgressBGPExportLabel(nodeObj)
		if expectExported {
			expectEgressIPHostRouteInFRR(nodeObj, ip)
		} else {
			expectEgressIPNotExportedInFRR(ip)
		}
	}
}

const bgpEgressIPMinWorkerNodes = 3

func schedulableWorkerNodes(cs clientset.Interface) ([]corev1.Node, int, error) {
	all, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
	if err != nil {
		return nil, 0, err
	}
	var workers []corev1.Node
	for _, node := range all.Items {
		if !isControlPlaneNode(node) {
			workers = append(workers, node)
		}
	}
	return workers, len(all.Items), nil
}

func skipUnlessEnoughWorkerNodesForBGPEgressIP(cs clientset.Interface) {
	workers, schedulable, err := schedulableWorkerNodes(cs)
	framework.ExpectNoError(err)
	if len(workers) < bgpEgressIPMinWorkerNodes {
		e2eskipper.Skipf("need at least %d worker nodes for BGP egress IP tests, got %d (schedulable nodes: %d)",
			bgpEgressIPMinWorkerNodes, len(workers), schedulable)
	}
}

func pickBGPEgressIPNodeSet(cs clientset.Interface) (bgpEgressIPNodeSet, error) {
	workers, _, err := schedulableWorkerNodes(cs)
	if err != nil {
		return bgpEgressIPNodeSet{}, err
	}
	if len(workers) < bgpEgressIPMinWorkerNodes {
		e2eskipper.Skipf("need at least %d worker nodes for BGP egress IP tests, got %d",
			bgpEgressIPMinWorkerNodes, len(workers))
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })
	return bgpEgressIPNodeSet{
		remote:  workers[0],
		egressA: workers[1],
		egressB: workers[2],
	}, nil
}

func ensureBGPEgressIPHealthCheckReachable(nodeNames ...string) {
	for _, nodeName := range nodeNames {
		allowOrDropNodeInputTrafficOnPort("Allow", nodeName, "tcp", bgpEgressIPGRPCHealthCheckPort)
	}
}

// ensureBGPEgressIPHealthCheckPortSynced aligns ovnkube-node and cluster-manager on the gRPC health-check port (egressip.go).
func ensureBGPEgressIPHealthCheckPortSynced(cs clientset.Interface) {
	ovnNS := deploymentconfig.Get().OVNKubernetesNamespace()
	portNode := getTemplateContainerEnv(ovnNS, "daemonset/ovnkube-node", getNodeContainerName(), OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)
	portControlPlane := getTemplateContainerEnv(ovnNS, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)
	want := DEFAULT_OVN_EGRESSIP_GRPC_HEALTHCHECK_PORT
	if portNode == want && portControlPlane == want {
		return
	}
	framework.Logf("syncing EgressIP gRPC health-check port to %s on ovnkube-node and ovnkube-cluster-manager (node=%q, control-plane=%q)", want, portNode, portControlPlane)
	setEnv := map[string]string{OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME: want}
	setUnsetTemplateContainerEnv(cs, ovnNS, "daemonset/ovnkube-node", getNodeContainerName(), setEnv)
	setUnsetTemplateContainerEnv(cs, ovnNS, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", setEnv)
}

// ensureBGPEgressIPReachabilityTimeoutForTests disables egress node gRPC reachability checks once per process.
// Kind often passes a TCP probe to :9107 while cluster-manager gRPC health checks still fail; timeout 0 is supported by ovn-kubernetes for this case.
func ensureBGPEgressIPReachabilityTimeoutForTests(cs clientset.Interface) {
	if egressIPReachabilityDisabledInOVNKubeConfig(cs) {
		return
	}
	current, configured := egressIPReachabilityTotalTimeoutFromConfig(cs)
	if configured && current == 0 {
		return
	}
	framework.Logf("setting egressip-reachability-total-timeout=0 in ovnkube-config for BGP EgressIP tests (configured=%v, current=%d)", configured, current)
	patchOVNKubeConfigEgressIPReachabilityTimeout(cs, 0)
}

func egressIPReachabilityTotalTimeoutFromConfig(cs clientset.Interface) (int, bool) {
	conf, err := ovnkubeConfigContents(cs)
	if err != nil {
		return 0, false
	}
	re := regexp.MustCompile(`(?m)^\s*egressip-reachability-total-timeout\s*=\s*(\d+)\s*$`)
	matches := re.FindStringSubmatch(conf)
	if len(matches) != 2 {
		return 0, false
	}
	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, false
	}
	return value, true
}

func egressIPReachabilityDisabledInOVNKubeConfig(cs clientset.Interface) bool {
	conf, err := ovnkubeConfigContents(cs)
	if err != nil {
		framework.Logf("could not read ovnkube-config for egress IP reachability: %v", err)
		return false
	}
	re := regexp.MustCompile(`(?m)^\s*egressip-reachability-total-timeout\s*=\s*0\s*$`)
	return re.MatchString(conf)
}

func ovnkubeConfigContents(cs clientset.Interface) (string, error) {
	ovnNS := deploymentconfig.Get().OVNKubernetesNamespace()
	cm, err := cs.CoreV1().ConfigMaps(ovnNS).Get(context.TODO(), "ovnkube-config", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	conf, ok := cm.Data["ovnkube.conf"]
	if !ok {
		return "", fmt.Errorf("ovnkube.conf missing in configmap ovnkube-config")
	}
	return conf, nil
}

func patchOVNKubeConfigEgressIPReachabilityTimeout(cs clientset.Interface, timeout int) {
	ovnNS := deploymentconfig.Get().OVNKubernetesNamespace()
	cm, err := cs.CoreV1().ConfigMaps(ovnNS).Get(context.TODO(), "ovnkube-config", metav1.GetOptions{})
	framework.ExpectNoError(err)
	conf := cm.Data["ovnkube.conf"]
	line := fmt.Sprintf("    egressip-reachability-total-timeout = %d", timeout)
	reLine := regexp.MustCompile(`(?m)^\s*egressip-reachability-total-timeout\s*=.*$`)
	if reLine.MatchString(conf) {
		conf = reLine.ReplaceAllString(conf, line)
	} else if strings.Contains(conf, "[ovnkubernetesfeature]") {
		conf = strings.Replace(conf, "[ovnkubernetesfeature]", "[ovnkubernetesfeature]\n"+line, 1)
	} else {
		conf = strings.TrimRight(conf, "\n") + "\n\n    [ovnkubernetesfeature]\n" + line + "\n"
	}
	cm.Data["ovnkube.conf"] = conf
	_, err = cs.CoreV1().ConfigMaps(ovnNS).Update(context.TODO(), cm, metav1.UpdateOptions{})
	framework.ExpectNoError(err)
	err = updateAndWaitForRollout(cs, ovnNS, "deployment/ovnkube-control-plane", 0, rolloutTimeout, func() {
		e2ekubectl.RunKubectlOrDie(ovnNS, "rollout", "restart", "deployment/ovnkube-control-plane")
	})
	framework.ExpectNoError(err)
}

const egressAssignableLabel = "k8s.ovn.org/egress-assignable"

func waitEgressNodesPrerequisitesForEgressIP(cs clientset.Interface, nodeNames ...string) {
	ensureBGPEgressIPHealthCheckReachable(nodeNames...)
	waitNodesHostCIDRsForEgressIP(cs, nodeNames...)
}

// prepareBGPEgressNodesForEgressIP labels egress nodes immediately before creating an EgressIP.
// Clustermanager only calls addEgressNode (which reconciles pending EgressIPs) when the label is
// applied while the node is ready, reachable on the health-check port, and has host-cidrs.
func prepareBGPEgressNodesForEgressIP(cs clientset.Interface, nodeNames ...string) {
	ensureBGPEgressIPHealthCheckReachable(nodeNames...)
	for _, nodeName := range nodeNames {
		labelEgressAssignable(cs, nodeName)
		e2enode.ExpectNodeHasLabel(context.TODO(), cs, nodeName, egressAssignableLabel, "dummy")
	}
}

// refreshEgressAssignableNodeLabels bumps the egress-assignable label so cluster-manager re-runs addEgressNode.
func refreshEgressAssignableNodeLabels(cs clientset.Interface, nodeNames ...string) {
	for _, nodeName := range nodeNames {
		e2enode.AddOrUpdateLabelOnNode(cs, nodeName, egressAssignableLabel, framework.RandomSuffix())
		e2enode.AddOrUpdateLabelOnNode(cs, nodeName, egressAssignableLabel, "dummy")
	}
}

// triggerEgressIPObjectReconcile bumps the EgressIP object so WatchEgressIP reconciles pending assignments.
func triggerEgressIPObjectReconcile(eipName string) {
	_, err := e2ekubectl.RunKubectl("default", "annotate", "eip", eipName,
		"k8s.ovn.org/e2e-reconcile-trigger="+framework.RandomSuffix(), "--overwrite")
	framework.ExpectNoError(err)
}

func waitNodesHostCIDRsForEgressIP(cs clientset.Interface, nodeNames ...string) {
	for _, nodeName := range nodeNames {
		gomega.Eventually(func() error {
			node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			hostCIDRs, err := util.ParseNodeHostCIDRsDropNetMask(node)
			if err != nil {
				return fmt.Errorf("node %s host-cidrs not parseable: %w", nodeName, err)
			}
			if hostCIDRs.Len() == 0 {
				return fmt.Errorf("node %s has empty host-cidrs", nodeName)
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
	}
}

func nodesFromSet(set bgpEgressIPNodeSet) []corev1.Node {
	return []corev1.Node{set.remote, set.egressA, set.egressB}
}

func nodeByName(nodes []corev1.Node, name string) corev1.Node {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	framework.Failf("node %s not found in test node set", name)
	return corev1.Node{}
}

func labelEgressAssignable(cs clientset.Interface, nodeName string) {
	e2enode.AddOrUpdateLabelOnNode(cs, nodeName, "k8s.ovn.org/egress-assignable", "dummy")
}

func unlabelEgressAssignable(cs clientset.Interface, nodeName string) {
	e2enode.RemoveLabelOffNode(cs, nodeName, "k8s.ovn.org/egress-assignable")
}

func labelBGPEgressExportOnNode(cs clientset.Interface, nodeName string) {
	e2enode.AddOrUpdateLabelOnNode(cs, nodeName, egressBGPExportLabelKey, egressBGPExportLabelValue)
}

func unlabelBGPEgressExportFromNode(cs clientset.Interface, nodeName string) {
	e2enode.RemoveLabelOffNode(cs, nodeName, egressBGPExportLabelKey)
}

// prepareBGPEgressIPFailoverRemote prepares the remote worker as a failover target (case 2/4 and single-stack RA specs).
// Case 2: raNodeSelectorMatch false — egress-assignable only. Case 4: true — egress-assignable plus egress-nodes=bgp.
func prepareBGPEgressIPFailoverRemote(f *framework.Framework, remoteName string, raNodeSelectorMatch bool) {
	cs := f.ClientSet
	if raNodeSelectorMatch {
		ginkgo.By(fmt.Sprintf("Labeling remote node %s egress-assignable and RouteAdvertisements nodeSelector match for failover", remoteName))
	} else {
		ginkgo.By(fmt.Sprintf("Labeling remote node %s egress-assignable without RouteAdvertisements nodeSelector match for failover", remoteName))
	}
	ginkgo.DeferCleanup(func() { unlabelEgressAssignable(cs, remoteName) })
	if raNodeSelectorMatch {
		ginkgo.DeferCleanup(func() { unlabelBGPEgressExportFromNode(cs, remoteName) })
	}
	registerEgressAssignableNodeWithClusterManager(f, remoteName)
	if raNodeSelectorMatch {
		labelBGPEgressExportOnNode(cs, remoteName)
	} else {
		unlabelBGPEgressExportFromNode(cs, remoteName)
	}
	assertNodeHasEgressAssignableLabel(cs, remoteName)
}

// registerEgressAssignableNodeWithClusterManager labels a node so cluster-manager runs addEgressNode.
// Health-check port must be open before the label is applied; CM only registers on label transition when ready+reachable.
func registerEgressAssignableNodeWithClusterManager(f *framework.Framework, nodeName string) {
	ensureBGPEgressIPHealthCheckReachable(nodeName)
	waitNodesHostCIDRsForEgressIP(f.ClientSet, nodeName)
	unlabelEgressAssignable(f.ClientSet, nodeName)
	labelNodeForEgress(f, nodeName)
}

func assertNodeHasEgressAssignableLabel(cs clientset.Interface, nodeName string) {
	e2enode.ExpectNodeHasLabel(context.TODO(), cs, nodeName, egressAssignableLabel, "dummy")
}

// waitForClusterManagerEgressFailoverTarget gives cluster-manager time to register the failover target node
// before we unlabel the source node (otherwise CM reports NoMatchingNodeFound for the freed spec IP).
func waitForClusterManagerEgressFailoverTarget(f *framework.Framework, eipName, targetNode string) {
	gomega.Eventually(func() error {
		registerEgressAssignableNodeWithClusterManager(f, targetNode)
		assertNodeHasEgressAssignableLabel(f.ClientSet, targetNode)
		triggerEgressIPObjectReconcile(eipName)
		node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), targetNode, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
				return fmt.Errorf("node %s is not Ready", targetNode)
			}
		}
		return nil
	}, 2*time.Minute, 3*time.Second).Should(gomega.Succeed())
}

// primaryNodeInternalIP matches egressip.go: dual-stack nodes use IPv4; otherwise use the node's family.
func primaryNodeInternalIP(node corev1.Node) string {
	v4Addrs := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv4Protocol)
	v6Addrs := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv6Protocol)
	if len(v4Addrs) > 0 && len(v6Addrs) > 0 {
		return v4Addrs[0]
	}
	if len(v6Addrs) > 0 {
		return v6Addrs[0]
	}
	if len(v4Addrs) > 0 {
		return v4Addrs[0]
	}
	return ""
}

const (
	bgpEgressIPHostSubnetIPOffset = 200
	bgpEgressIPHostSubnetIPEnd    = 254
)

type egressIPListWithSpec struct {
	Items []struct {
		Spec struct {
			EgressIPs []string `json:"egressIPs"`
		} `json:"spec"`
	} `json:"items"`
}

func clusterEgressIPSpecAddresses() (sets.Set[string], error) {
	stdout, err := e2ekubectl.RunKubectl("", "get", "eip", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	list := egressIPListWithSpec{}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		return nil, err
	}
	occupied := sets.New[string]()
	for _, item := range list.Items {
		for _, ip := range item.Spec.EgressIPs {
			occupied.Insert(ip)
		}
	}
	return occupied, nil
}

// bgpDualStackEgressIPPlan drives dual-stack placement and failover for BGP egress IP e2e.
type bgpDualStackEgressIPPlan struct {
	wantV4On      string
	wantV6On      string
	// placementOnRASelectorMatch: nil = default (v4/v6 on the two egress workers); true = both addresses on
	// nodes matching the EgressIP RouteAdvertisements nodeSelector; false = both on non-matching nodes.
	placementOnRASelectorMatch *bool
	labelAtCreate                  []string
	failoverFromAdvertised         bool
	failoverRemoteMatchesRASelector bool
	failoverTo string
}

func bgpDualStackEgressIPPlanForScenario(scenario bgpEgressIPScenario, nodeSet bgpEgressIPNodeSet) bgpDualStackEgressIPPlan {
	match := true
	noMatch := false
	// Case 2: both spec IPs on RA-matching egress workers; remote stays out of the assignable pool until
	// failover (so it does not already host the other spec IP). CM uses one address per node per EgressIP.
	if scenario.useRouteAdvertisementsNodeSelector && scenario.failoverFromAdvertised {
		return bgpDualStackEgressIPPlan{
			wantV4On:                        nodeSet.egressA.Name,
			wantV6On:                        nodeSet.egressA.Name,
			placementOnRASelectorMatch:      &match,
			labelAtCreate:                   []string{nodeSet.egressA.Name, nodeSet.egressB.Name}, // both workers match RA nodeSelector
			failoverFromAdvertised:          true,
			failoverRemoteMatchesRASelector: false,
			failoverTo:                      nodeSet.remote.Name,
		}
	}
	// Case 5 (L3 + RA nodeSelector): spec IPs on non-matching egress workers; remote gets egress-assignable + match label at failover.
	if scenario.useRouteAdvertisementsNodeSelector && !scenario.failoverFromAdvertised {
		return bgpDualStackEgressIPPlan{
			wantV4On:                        nodeSet.egressB.Name,
			wantV6On:                        nodeSet.egressB.Name,
			placementOnRASelectorMatch:      &noMatch,
			labelAtCreate:                   []string{nodeSet.egressA.Name, nodeSet.egressB.Name}, // neither has match label in case 4
			failoverFromAdvertised:          false,
			failoverRemoteMatchesRASelector: true,
			failoverTo:                      nodeSet.remote.Name,
		}
	}
	// Cases 1 and 4 (no RA nodeSelector): v4@egressA, v6@egressB; failover one address from an egress worker→remote.
	return bgpDualStackEgressIPPlan{
		wantV4On:      nodeSet.egressA.Name,
		wantV6On:      nodeSet.egressB.Name,
		labelAtCreate: []string{nodeSet.egressA.Name, nodeSet.egressB.Name},
		failoverTo:    nodeSet.remote.Name,
	}
}

// allocateBGPEgressTestIPsFromMachineNetwork matches egressip.go machine-network allocators.
func allocateBGPEgressTestIPsFromMachineNetwork(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, scenario bgpEgressIPScenario) ([]string, bgpDualStackEgressIPPlan) {
	var egressIPs []string
	plan := bgpDualStackEgressIPPlan{}
	gomega.Eventually(func() error {
		occupied, err := clusterEgressIPSpecAddresses()
		if err != nil {
			return err
		}
		if isIPv4Supported(cs) && isIPv6Supported(cs) {
			plan = bgpDualStackEgressIPPlanForScenario(scenario, nodeSet)
			return allocateDualStackBGPEgressTestIPs(cs, nodeSet, plan, occupied, &egressIPs)
		}
		return allocateSingleStackBGPEgressTestIPs(cs, nodeSet, occupied, &egressIPs)
	}, 2*time.Minute, time.Second).Should(gomega.Succeed())
	return egressIPs, plan
}

func allocateDualStackBGPEgressTestIPs(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, plan bgpDualStackEgressIPPlan, occupied sets.Set[string], egressIPs *[]string) error {
	v4Alloc, v6Alloc := plan.wantV4On, plan.wantV6On
	if plan.wantV4On == plan.wantV6On {
		v4Alloc = plan.wantV4On
		v6Alloc = plan.wantV4On
	}
	maxAttempts := int(bgpEgressIPHostSubnetIPEnd - bgpEgressIPHostSubnetIPOffset + 1)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		v4IP, err := allocators.NewPrimaryIPv4(v4Alloc)
		if err != nil {
			break
		}
		v6IP, err := allocators.NewPrimaryIPv6(v6Alloc)
		if err != nil {
			break
		}
		v4Str, v6Str := v4IP.String(), v6IP.String()
		if occupied.Has(v4Str) || occupied.Has(v6Str) {
			continue
		}
		if !bgpDualStackEgressIPAssignable(cs, nodeSet, v4Str, v6Str, plan.failoverTo) {
			continue
		}
		*egressIPs = []string{v4Str, v6Str}
		framework.Logf("allocated BGP dual-stack egress test IPs %s and %s (plan v4→%s v6→%s, failover to %s)",
			v4Str, v6Str, plan.wantV4On, plan.wantV6On, plan.failoverTo)
		return nil
	}
	return fmt.Errorf("no dual-stack machine-network egress IPs for plan v4→%s v6→%s (occupied: %v)",
		plan.wantV4On, plan.wantV6On, sets.List(occupied))
}

func bgpDualStackEgressIPAssignable(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, v4, v6, failoverTo string) bool {
	if !bgpEgressIPAssignableOnBothEgressNodes(cs, nodeSet, v4) || !bgpEgressIPAssignableOnBothEgressNodes(cs, nodeSet, v6) {
		return false
	}
	return bgpEgressIPAssignableOnNodeByName(cs, failoverTo, v4) && bgpEgressIPAssignableOnNodeByName(cs, failoverTo, v6)
}

func dualStackBGPEgressIPv4AndIPv6(egressIPs []string) (v4 string, v6 string) {
	for _, ip := range egressIPs {
		if utilnet.IsIPv4String(ip) {
			v4 = ip
		} else {
			v6 = ip
		}
	}
	if v4 == "" || v6 == "" {
		framework.Failf("expected dual-stack egress IP spec to contain IPv4 and IPv6, got %v", egressIPs)
	}
	return v4, v6
}

func waitDualStackBGPEgressIPPlacement(cs clientset.Interface, eipName, v4, v6 string, plan bgpDualStackEgressIPPlan) {
	if plan.placementOnRASelectorMatch != nil {
		waitDualStackBGPEgressIPOnRASelectorNodes(cs, eipName, v4, v6, *plan.placementOnRASelectorMatch)
		return
	}
	gomega.Eventually(func() error {
		assignments := egressIPAssignmentsByAddress(getEgressIPStatusItems(eipName))
		if assignments[v4] != plan.wantV4On {
			return fmt.Errorf("IPv4 egress IP %s on %q, want %q", v4, assignments[v4], plan.wantV4On)
		}
		if assignments[v6] != plan.wantV6On {
			return fmt.Errorf("IPv6 egress IP %s on %q, want %q", v6, assignments[v6], plan.wantV6On)
		}
		return nil
	}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed())
}

func waitDualStackBGPEgressIPOnRASelectorNodes(cs clientset.Interface, eipName, v4, v6 string, mustMatchRASelector bool) {
	gomega.Eventually(func() error {
		assignments := egressIPAssignmentsByAddress(getEgressIPStatusItems(eipName))
		for _, ip := range []string{v4, v6} {
			nodeName := assignments[ip]
			node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			matches := nodeHasEgressBGPExportLabel(*node)
			if matches != mustMatchRASelector {
				return fmt.Errorf("egress IP %s on %q (RA nodeSelector match=%v), want match=%v",
					ip, nodeName, matches, mustMatchRASelector)
			}
		}
		if len(assignments) == 2 && assignments[v4] == assignments[v6] {
			return fmt.Errorf("dual-stack egress IPs must be on distinct nodes (both on %q)", assignments[v4])
		}
		return nil
	}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed())
}

func pickDualStackFailoverFromEgressWorkers(cs clientset.Interface, assignments map[string]string, nodeSet bgpEgressIPNodeSet, failoverToNode string) (string, string) {
	for _, onNode := range []string{nodeSet.egressA.Name, nodeSet.egressB.Name} {
		if ip, node, ok := pickFailoverIPOnNode(cs, assignments, nodeSet, onNode, false, failoverToNode); ok {
			return ip, node
		}
		if ip, node, ok := pickFailoverIPOnNode(cs, assignments, nodeSet, onNode, true, failoverToNode); ok {
			return ip, node
		}
	}
	framework.Failf("no egress IP on egress workers %s or %s can fail over to %s", nodeSet.egressA.Name, nodeSet.egressB.Name, failoverToNode)
	return "", ""
}

func pickDualStackFailoverForRAPlacement(cs clientset.Interface, assignments map[string]string, nodeSet bgpEgressIPNodeSet, fromAdvertised bool, failoverToNode string) (string, string) {
	if ip, node, ok := pickFailoverIPOnRASelectorPlacement(cs, assignments, nodeSet, fromAdvertised, false, failoverToNode); ok {
		return ip, node
	}
	if ip, node, ok := pickFailoverIPOnRASelectorPlacement(cs, assignments, nodeSet, fromAdvertised, true, failoverToNode); ok {
		return ip, node
	}
	role := "RA nodeSelector-matching"
	if !fromAdvertised {
		role = "non-matching"
	}
	framework.Failf("no egress IP on a %s node can fail over to %s", role, failoverToNode)
	return "", ""
}

func pickFailoverIPOnRASelectorPlacement(cs clientset.Interface, assignments map[string]string, nodeSet bgpEgressIPNodeSet, fromAdvertised bool, ipv4Only bool, failoverToNode string) (string, string, bool) {
	for ip, nodeName := range assignments {
		if ipv4Only && !utilnet.IsIPv4String(ip) {
			continue
		}
		node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if nodeHasEgressBGPExportLabel(*node) != fromAdvertised {
			continue
		}
		if !bgpEgressIPAssignableOnFailoverTarget(cs, nodeSet, nodeName, ip, failoverToNode) {
			continue
		}
		return ip, nodeName, true
	}
	return "", "", false
}

func bgpEgressIPAssignableOnBothEgressNodes(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, ipStr string) bool {
	for _, nodeName := range []string{nodeSet.egressA.Name, nodeSet.egressB.Name} {
		node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil || !egressIPAssignableOnNode(node, ipStr) {
			return false
		}
	}
	return true
}

func bgpEgressIPAssignableOnNodeByName(cs clientset.Interface, nodeName, ipStr string) bool {
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return egressIPAssignableOnNode(node, ipStr)
}

func bgpEgressIPAssignableOnSiblingEgressNode(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, assignedNode, ipStr string) bool {
	return bgpEgressIPAssignableOnNodeByName(cs, siblingBGPEgressNode(nodeSet, assignedNode), ipStr)
}

func bgpEgressIPAssignableOnFailoverTarget(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, assignedNode, ipStr, failoverToNode string) bool {
	if failoverToNode != "" {
		return bgpEgressIPAssignableOnNodeByName(cs, failoverToNode, ipStr)
	}
	return bgpEgressIPAssignableOnSiblingEgressNode(cs, nodeSet, assignedNode, ipStr)
}

func allocateSingleStackBGPEgressTestIPs(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, occupied sets.Set[string], egressIPs *[]string) error {
	egressNodes := []corev1.Node{nodeSet.egressA, nodeSet.egressB}
	for _, egressNode := range egressNodes {
		node, err := cs.CoreV1().Nodes().Get(context.TODO(), egressNode.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		nodeIP := primaryNodeInternalIP(*node)
		if nodeIP == "" {
			continue
		}
		for range bgpEgressIPHostSubnetIPEnd - bgpEgressIPHostSubnetIPOffset + 1 {
			var ip net.IP
			var allocErr error
			if utilnet.IsIPv6String(nodeIP) {
				ip, allocErr = allocators.NewPrimaryIPv6(node.Name)
			} else {
				ip, allocErr = allocators.NewPrimaryIPv4(node.Name)
			}
			if allocErr != nil {
				return allocErr
			}
			candidate := ip.String()
			if occupied.Has(candidate) {
				continue
			}
			if !egressIPAssignableOnNode(node, candidate) {
				continue
			}
			*egressIPs = []string{candidate}
			framework.Logf("allocated BGP egress test IP %s via machine-network allocator on node %s (internal IP %s)", candidate, node.Name, nodeIP)
			return nil
		}
	}
	return fmt.Errorf("no assignable machine-network egress IP on nodes %s/%s (occupied: %v)", nodeSet.egressA.Name, nodeSet.egressB.Name, sets.List(occupied))
}

func egressIPAssignableOnNode(node *corev1.Node, ipStr string) bool {
	return egressIPAssignableOnNodeForNetwork(node, ipStr, types.DefaultNetworkName)
}

func egressIPAssignableOnNodeForNetwork(node *corev1.Node, ipStr, _ string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	eIPConfig, err := util.GetNodeEIPConfig(node)
	if err != nil {
		return false
	}
	network, err := util.GetEgressIPNetwork(node, eIPConfig, ip)
	return err == nil && network != ""
}

func waitBGPEgressCUDNHostSubnetsOnNodes(cs clientset.Interface, networkName string, nodeNames ...string) {
	for _, nodeName := range nodeNames {
		gomega.Eventually(func() error {
			node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			subnets, err := getHostSubnetsForNodeOnNetwork(node, networkName)
			if err != nil {
				return err
			}
			if len(subnets) == 0 {
				return fmt.Errorf("node %s has no host subnets for network %s", nodeName, networkName)
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
	}
}

func getHostSubnetsForNodeOnNetwork(node *corev1.Node, networkName string) ([]string, error) {
	if networkName == "" || networkName == types.DefaultNetworkName {
		return nil, fmt.Errorf("network name is required for non-default host subnet lookup")
	}
	ipNets, err := util.ParseNodeHostSubnetAnnotation(node, networkName)
	if err != nil {
		return nil, err
	}
	hostSubnets := make([]string, 0, len(ipNets))
	for _, ipNet := range ipNets {
		hostSubnets = append(hostSubnets, ipNet.String())
	}
	return hostSubnets, nil
}

func bgpE2ERouteAdvertisementFRRConfigurationSelector(raClient raclientset.Interface) metav1.LabelSelector {
	defaultRAName := defaultNetworkRouteAdvertisementName()
	ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), defaultRAName, metav1.GetOptions{})
	if err == nil {
		return ra.Spec.FRRConfigurationSelector
	}
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			bgpE2EFRRConfigurationLabelKey: bgpE2EFRRConfigurationLabelValue,
		},
	}
}

func waitBGPClusterReadyForRouteAdvertisements() {
	ensureBGPReceiveAllFRRConfiguration()
	frrNS := deploymentconfig.Get().FRRK8sNamespace()
	labelSelector := bgpE2EFRRConfigurationLabelKey + "=" + bgpE2EFRRConfigurationLabelValue
	gomega.Eventually(func() string {
		out, err := e2ekubectl.RunKubectl(frrNS, "get", "frrconfigurations", "-l", labelSelector, "-o", "name")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}, 2*time.Minute, time.Second).ShouldNot(gomega.BeEmpty(), "expected at least one FRRConfiguration with label %s in namespace %s", labelSelector, frrNS)
}

// ensureBGPReceiveAllFRRConfiguration applies the Kind-style receive-all FRRConfiguration when
// cluster install created the default RouteAdvertisements before configure_frr_k8s ran.
func ensureBGPReceiveAllFRRConfiguration() {
	frrNS := deploymentconfig.Get().FRRK8sNamespace()
	labelSelector := bgpE2EFRRConfigurationLabelKey + "=" + bgpE2EFRRConfigurationLabelValue
	if out, err := e2ekubectl.RunKubectl(frrNS, "get", "frrconfigurations", "-l", labelSelector, "-o", "name"); err == nil && strings.TrimSpace(out) != "" {
		return
	}
	providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
	framework.ExpectNoError(err)
	externalContainerNetInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(
		infraapi.ExternalContainer{Name: routerContainerName}, providerPrimaryNetwork)
	framework.ExpectNoError(err)
	var neighborIPs []string
	if externalContainerNetInf.IPv4 != "" {
		neighborIPs = append(neighborIPs, externalContainerNetInf.IPv4)
	}
	if externalContainerNetInf.IPv6 != "" {
		neighborIPs = append(neighborIPs, externalContainerNetInf.IPv6)
	}
	gomega.Expect(neighborIPs).NotTo(gomega.BeEmpty(), "external FRR router must have at least one neighbor IP for BGP")

	manifest := receiveAllFRRConfigurationManifest(neighborIPs)
	tmp := filepath.Join(os.TempDir(), "bgp-receive-all-"+framework.RandomSuffix()+".yaml")
	framework.ExpectNoError(os.WriteFile(tmp, []byte(manifest), 0o644))
	_, err = e2ekubectl.RunKubectl(frrNS, "apply", "-f", tmp)
	framework.ExpectNoError(err)
}

func receiveAllFRRConfigurationManifest(neighborIPs []string) string {
	var b strings.Builder
	b.WriteString(`apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: receive-all
  labels:
    name: receive-all
spec:
  bgp:
    routers:
    - asn: 64512
      neighbors:
`)
	for _, ip := range neighborIPs {
		b.WriteString(fmt.Sprintf(`      - address: %s
        asn: 64512
        disableMP: true
        toReceive:
          allowed:
            mode: all
`, ip))
	}
	return b.String()
}

func createBGPL3CUDNPodNetworkRouteAdvertisement(raClient raclientset.Interface, name, cudnMatchLabel string, frrSelector metav1.LabelSelector) {
	ra := &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: rav1.RouteAdvertisementsSpec{
			NetworkSelectors: apitypes.NetworkSelectors{
				{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{cudnMatchLabel: ""},
						},
					},
				},
			},
			NodeSelector:             metav1.LabelSelector{},
			FRRConfigurationSelector: frrSelector,
			Advertisements:           []rav1.AdvertisementType{rav1.PodNetwork},
		},
	}
	_, err := raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
	framework.ExpectNoError(err)
}

func createBGPEgressIPRouteAdvertisement(raClient raclientset.Interface, name, cudnMatchLabel string, nodeSelector, frrSelector metav1.LabelSelector) {
	networkSelectors := apitypes.NetworkSelectors{
		{NetworkSelectionType: apitypes.DefaultNetwork},
	}
	if cudnMatchLabel != "" {
		networkSelectors = apitypes.NetworkSelectors{
			{
				NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
				ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
					NetworkSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{cudnMatchLabel: ""},
					},
				},
			},
		}
	}
	ra := &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: rav1.RouteAdvertisementsSpec{
			NetworkSelectors:         networkSelectors,
			NodeSelector:             nodeSelector,
			FRRConfigurationSelector: frrSelector,
			Advertisements:           []rav1.AdvertisementType{rav1.EgressIP},
		},
	}
	_, err := raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
	framework.ExpectNoError(err)
}

func waitRouteAdvertisementAccepted(raClient raclientset.Interface, name string) {
	gomega.Eventually(func() error {
		ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		condition := meta.FindStatusCondition(ra.Status.Conditions, rav1.RouteAdvertisementsAccepted)
		if condition == nil {
			return fmt.Errorf("no Accepted condition on RouteAdvertisements %s", name)
		}
		if condition.Status != metav1.ConditionTrue {
			return fmt.Errorf("RouteAdvertisements %s not accepted: %s", name, condition.Message)
		}
		return nil
	}, 2*time.Minute, time.Second).Should(gomega.Succeed())
}

func waitRouteAdvertisementConfigurationError(raClient raclientset.Interface, name, messageFragment string) {
	gomega.Eventually(func() bool {
		ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		condition := meta.FindStatusCondition(ra.Status.Conditions, rav1.RouteAdvertisementsAccepted)
		if condition == nil {
			return false
		}
		return condition.Reason == "ConfigurationError" && strings.Contains(condition.Message, messageFragment)
	}, 2*time.Minute, time.Second).Should(gomega.BeTrue())
}

func applyEgressIPFromManifest(manifest string) {
	tmp := filepath.Join(os.TempDir(), "bgp-eip-"+framework.RandomSuffix()+".yaml")
	framework.ExpectNoError(os.WriteFile(tmp, []byte(manifest), 0o644))
	defer func() { _ = os.Remove(tmp) }()
	e2ekubectl.RunKubectlOrDie("default", "create", "-f", tmp)
}

func getEgressIPStatusItems(eipName string) []egressIPStatus {
	egressIP := egressIP{}
	stdout, err := e2ekubectl.RunKubectl("default", "get", "eip", eipName, "-o", "json")
	if err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(stdout), &egressIP); err != nil {
		framework.Failf("failed to unmarshal egress IP %s: %v", eipName, err)
	}
	return egressIP.Status.Items
}

func waitForEgressIPAssignments(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, eipName string, expectedIPs []string, requireDistinctNodes bool) map[string]string {
	var assignments map[string]string
	var pollCount int
	err := wait.PollImmediate(retryInterval, bgpEgressIPPollTimeout, func() (bool, error) {
		pollCount++
		items := getEgressIPStatusItems(eipName)
		assignments = egressIPAssignmentsByAddress(items)
		if len(items) != len(expectedIPs) {
			if pollCount == 1 || pollCount%8 == 0 {
				triggerEgressIPObjectReconcile(eipName)
			}
			if pollCount == 1 || pollCount%12 == 0 {
				refreshEgressAssignableNodeLabels(cs, nodeSet.egressA.Name, nodeSet.egressB.Name)
			}
			return false, nil
		}
		for _, ip := range expectedIPs {
			if assignments[ip] == "" {
				return false, nil
			}
		}
		if requireDistinctNodes && len(assignments) == 2 {
			nodes := setsFromAssignments(assignments)
			if len(nodes) != 2 {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		dumpBGPEgressIPDiagnostics(cs, nodeSet, eipName)
		framework.Failf("timed out waiting for egress IP assignments on %s (want %v): last map %v (check cluster-manager logs for %q and %q; ensure egressip-reachability-total-timeout=0 in ovnkube-config and OVN_EGRESSIP_HEALTHCHECK_PORT=9107 on ovnkube-cluster-manager)",
			eipName, expectedIPs, assignments, eipName, "not ready and reachable")
	}
	return assignments
}

// ensureOVNEgressIPFeatureEnabled verifies cluster-manager runs with EgressIP enabled (required for status assignment).
func ensureOVNEgressIPFeatureEnabled(cs clientset.Interface) {
	ovnNS := deploymentconfig.Get().OVNKubernetesNamespace()
	deployment, err := cs.AppsV1().Deployments(ovnNS).Get(context.TODO(), "ovnkube-control-plane", metav1.GetOptions{})
	if err != nil {
		e2eskipper.Skipf("cannot verify EgressIP feature on deployment %s/ovnkube-control-plane: %v", ovnNS, err)
	}
	for _, c := range deployment.Spec.Template.Spec.Containers {
		if c.Name != "ovnkube-cluster-manager" {
			continue
		}
		for _, env := range c.Env {
			if env.Name == "OVN_EGRESSIP_ENABLE" && env.Value == "true" {
				return
			}
		}
	}
	e2eskipper.Skipf("OVN_EGRESSIP_ENABLE is not true on %s/ovnkube-control-plane (ovnkube-cluster-manager); EgressIP objects will not be assigned", ovnNS)
}

func dumpBGPEgressIPDiagnostics(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, eipName string) {
	ovnNS := deploymentconfig.Get().OVNKubernetesNamespace()
	if out, err := e2ekubectl.RunKubectl(ovnNS, "logs", "deployment/ovnkube-control-plane", "-c", "ovnkube-cluster-manager", "--tail=400"); err == nil {
		var relevant []string
		for _, line := range strings.Split(out, "\n") {
			lower := strings.ToLower(line)
			if strings.Contains(line, eipName) || strings.Contains(lower, "egressip") ||
				strings.Contains(lower, "assignable") || strings.Contains(lower, "reachable") {
				relevant = append(relevant, line)
			}
		}
		if len(relevant) > 0 {
			framework.Logf("ovnkube-cluster-manager egress IP log lines:\n%s", strings.Join(relevant, "\n"))
		} else {
			framework.Logf("ovnkube-cluster-manager logs (tail 400) contained no egress IP lines matching %q", eipName)
		}
	}
	if out, err := e2ekubectl.RunKubectl("default", "get", "events", "-A", "--field-selector", "involvedObject.name="+eipName); err == nil {
		framework.Logf("EgressIP %s events (all namespaces):\n%s", eipName, out)
	}
	if out, err := e2ekubectl.RunKubectl("default", "get", "nodes", "-l", egressAssignableLabel); err == nil {
		framework.Logf("nodes labeled %s:\n%s", egressAssignableLabel, out)
	}
	if out, err := e2ekubectl.RunKubectl("default", "describe", "eip", eipName); err == nil {
		framework.Logf("EgressIP %s describe:\n%s", eipName, out)
	}
	if out, err := e2ekubectl.RunKubectl("default", "get", "eip", eipName, "-o", "yaml"); err == nil {
		framework.Logf("EgressIP %s object:\n%s", eipName, out)
	}
	for _, nodeName := range []string{nodeSet.egressA.Name, nodeSet.egressB.Name} {
		node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil {
			framework.Logf("failed to get node %s for diagnostics: %v", nodeName, err)
			continue
		}
		ovnSubnets, ovnErr := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
		ovnSubnetStrs := make([]string, 0, len(ovnSubnets))
		if ovnErr == nil {
			for _, sn := range ovnSubnets {
				ovnSubnetStrs = append(ovnSubnetStrs, sn.String())
			}
		}
		framework.Logf("egress node %s: labels[%s]=%q host-cidrs=%q node-ifaddr=%q node-subnets-default=%v (parse err: %v)",
			nodeName, egressAssignableLabel, node.Labels[egressAssignableLabel],
			node.Annotations[util.OVNNodeHostCIDRs], node.Annotations[util.OvnNodeIfAddr], ovnSubnetStrs, ovnErr)
	}
}

func egressIPAssignmentsByAddress(items []egressIPStatus) map[string]string {
	m := make(map[string]string, len(items))
	for _, item := range items {
		m[item.EgressIP] = item.Node
	}
	return m
}

func setsFromAssignments(assignments map[string]string) map[string]string {
	nodes := map[string]string{}
	for _, node := range assignments {
		nodes[node] = node
	}
	return nodes
}

func assertBGPEgressIPAssignmentsOnDistinctEgressNodes(assignments map[string]string, egressA, egressB string) {
	for _, node := range assignments {
		gomega.Expect(node).To(gomega.Or(gomega.Equal(egressA), gomega.Equal(egressB)))
	}
	if len(assignments) == 2 {
		nodesUsed := setsFromAssignments(assignments)
		gomega.Expect(len(nodesUsed)).To(gomega.Equal(2), "dual-stack egress IPs should be assigned to different nodes")
	}
}

func siblingBGPEgressNode(nodeSet bgpEgressIPNodeSet, nodeName string) string {
	switch nodeName {
	case nodeSet.egressA.Name:
		return nodeSet.egressB.Name
	case nodeSet.egressB.Name:
		return nodeSet.egressA.Name
	default:
		framework.Failf("node %s is not an egress node in the test set", nodeName)
		return ""
	}
}

func waitForEgressIPFailover(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, eipName, failoverIP, fromNode, toNode string, unchanged map[string]string, expectedStatusCount int) map[string]string {
	var assignments map[string]string
	var pollCount int
	err := wait.PollImmediate(retryInterval, bgpEgressIPPollTimeout, func() (bool, error) {
		pollCount++
		items := getEgressIPStatusItems(eipName)
		assignments = egressIPAssignmentsByAddress(items)
		if expectedStatusCount > 0 && len(items) != expectedStatusCount {
			if pollCount == 1 || pollCount%6 == 0 {
				ensureBGPEgressIPHealthCheckReachable(toNode)
				refreshEgressAssignableNodeLabels(cs, toNode)
				triggerEgressIPObjectReconcile(eipName)
			}
			return false, nil
		}
		if assignments[failoverIP] != toNode {
			if pollCount == 1 || pollCount%6 == 0 {
				ensureBGPEgressIPHealthCheckReachable(toNode)
				refreshEgressAssignableNodeLabels(cs, toNode)
				triggerEgressIPObjectReconcile(eipName)
			}
			return false, nil
		}
		for ip, node := range unchanged {
			if ip == failoverIP {
				continue
			}
			if assignments[ip] != node {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		dumpBGPEgressIPDiagnostics(cs, nodeSet, eipName)
		framework.Failf("timed out waiting for egress IP %s to move from node %s to node %s (expected %d status items): %v",
			failoverIP, fromNode, toNode, expectedStatusCount, assignments)
	}
	return assignments
}

func pickFailoverTarget(cs clientset.Interface, assignments map[string]string, nodeSet bgpEgressIPNodeSet, scenario bgpEgressIPScenario, failoverToNode string) (string, string) {
	if scenario.useRouteAdvertisementsNodeSelector {
		if scenario.failoverFromAdvertised {
			if ip, node, ok := pickFailoverIPOnRASelectorPlacement(cs, assignments, nodeSet, true, true, failoverToNode); ok {
				return ip, node
			}
			if ip, node, ok := pickFailoverIPOnRASelectorPlacement(cs, assignments, nodeSet, true, false, failoverToNode); ok {
				return ip, node
			}
		} else {
			if ip, node, ok := pickFailoverIPOnRASelectorPlacement(cs, assignments, nodeSet, false, true, failoverToNode); ok {
				ginkgo.By("Selecting IPv4 egress IP on a node that does not match RouteAdvertisements nodeSelector")
				return ip, node
			}
			if ip, node, ok := pickFailoverIPOnRASelectorPlacement(cs, assignments, nodeSet, false, false, failoverToNode); ok {
				ginkgo.By("Selecting egress IP on a node that does not match RouteAdvertisements nodeSelector")
				return ip, node
			}
		}
		target := "sibling egress node"
		if failoverToNode != "" {
			target = failoverToNode
		}
		framework.Failf("no egress IP on the required egress node can fail over to %s", target)
		return "", ""
	}
	if ip, node, ok := pickFailoverIPPreferAssignable(cs, assignments, nodeSet, true, failoverToNode); ok {
		return ip, node
	}
	if ip, node, ok := pickFailoverIPPreferAssignable(cs, assignments, nodeSet, false, failoverToNode); ok {
		return ip, node
	}
	target := "sibling egress node"
	if failoverToNode != "" {
		target = failoverToNode
	}
	framework.Failf("no egress IP assignment found that can fail over to %s", target)
	return "", ""
}

func pickFailoverIPOnNode(cs clientset.Interface, assignments map[string]string, nodeSet bgpEgressIPNodeSet, onNode string, ipv4Only bool, failoverToNode string) (string, string, bool) {
	for ip, node := range assignments {
		if node != onNode {
			continue
		}
		if ipv4Only && !utilnet.IsIPv4String(ip) {
			continue
		}
		if !bgpEgressIPAssignableOnFailoverTarget(cs, nodeSet, node, ip, failoverToNode) {
			continue
		}
		return ip, node, true
	}
	return "", "", false
}

func pickFailoverIPPreferAssignable(cs clientset.Interface, assignments map[string]string, nodeSet bgpEgressIPNodeSet, ipv4Only bool, failoverToNode string) (string, string, bool) {
	for ip, node := range assignments {
		if ipv4Only && !utilnet.IsIPv4String(ip) {
			continue
		}
		if !bgpEgressIPAssignableOnFailoverTarget(cs, nodeSet, node, ip, failoverToNode) {
			continue
		}
		return ip, node, true
	}
	return "", "", false
}

func nodeHasEgressBGPExportLabel(node corev1.Node) bool {
	return node.Labels[egressBGPExportLabelKey] == egressBGPExportLabelValue
}

func hostRoutePrefix(egressIP string) string {
	if strings.Contains(egressIP, "/") {
		return egressIP
	}
	if utilnet.IsIPv6String(egressIP) {
		return egressIP + "/128"
	}
	return egressIP + "/32"
}

func nodeBGPNexthop(node corev1.Node, isIPv6 bool) string {
	if isIPv6 {
		lla, err := GetNodeIPv6LinkLocalAddressForEth0(node.Name)
		framework.ExpectNoError(err)
		return lla
	}
	ips := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv4Protocol)
	gomega.Expect(len(ips)).To(gomega.BeNumerically(">", 0))
	return ips[0]
}

func expectEgressIPHostRouteInFRR(node corev1.Node, egressIP string) {
	prefix := hostRoutePrefix(egressIP)
	isIPv6 := utilnet.IsIPv6String(egressIP)
	nexthop := nodeBGPNexthop(node, isIPv6)
	ipVer := ""
	if isIPv6 {
		ipVer = " -6"
	}
	externalContainer := infraapi.ExternalContainer{Name: routerContainerName}
	cmd := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, prefix), " ")

	gomega.Eventually(func() bool {
		routes, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, cmd)
		framework.ExpectNoError(err)
		return strings.Contains(routes, nexthop)
	}, 90*time.Second, 5*time.Second).Should(gomega.BeTrue(), "expected FRR route for %s via %s on node %s", prefix, nexthop, node.Name)
}

// expectEgressIPNotExportedInFRR asserts the egress IP host route is not present on the
// external FRR router (RA nodeSelector or post-failover withdraw).
func expectEgressIPNotExportedInFRR(egressIP string) {
	prefix := hostRoutePrefix(egressIP)
	isIPv6 := utilnet.IsIPv6String(egressIP)
	ipVer := ""
	if isIPv6 {
		ipVer = " -6"
	}
	externalContainer := infraapi.ExternalContainer{Name: routerContainerName}
	cmd := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, prefix), " ")

	gomega.Eventually(func() bool {
		routes, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, cmd)
		if err != nil {
			return false
		}
		return strings.TrimSpace(routes) == ""
	}, bgpEgressIPPollTimeout, 5*time.Second).Should(gomega.BeTrue(), "expected no FRR BGP route for egress IP prefix %s", prefix)
}

func assertEgressIPDatapathFromLocalAndRemotePods(ns, localPod, remotePod string, serverIPs []string, assignments map[string]string, onPrimaryCUDN bool) {
	for egressIP := range assignments {
		serverIP := bgpServerIPForFamily(serverIPs, egressIP)
		port := strconv.Itoa(netexecPort)
		curl := func(pod string) error {
			if onPrimaryCUDN {
				return curlAgnHostClientIPFromPodWithRetries(ns, pod, egressIP, serverIP, port)
			}
			return curlAgnHostClientIPFromPod(ns, pod, egressIP, serverIP, port)
		}
		gomega.Eventually(func() error {
			return curl(localPod)
		}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed())
		gomega.Eventually(func() error {
			return curl(remotePod)
		}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed())
	}
}

func curlAgnHostClientIPFromPodWithRetries(namespace, pod, expectedIP, dstIP, containerPort string) error {
	dst := net.JoinHostPort(dstIP, containerPort)
	cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s/clientip", dst)
	stdout, err := e2epodoutput.RunHostCmdWithRetries(namespace, pod, cmd, framework.Poll, bgpEgressIPPollTimeout)
	if err != nil {
		return fmt.Errorf("failed to curl agnhost on %s from %s, err: %w", dstIP, pod, err)
	}
	ourip, _, err := net.SplitHostPort(strings.TrimSpace(stdout))
	if err != nil {
		return fmt.Errorf("failed to split agnhost clientip response %q: %w", stdout, err)
	}
	if ourip != expectedIP {
		return fmt.Errorf("reached agnhost %s with ip %s from %s instead of %s", dstIP, ourip, pod, expectedIP)
	}
	return nil
}

func bgpServerIPForFamily(serverIPs []string, egressIP string) string {
	wantV6 := utilnet.IsIPv6String(egressIP)
	for _, ip := range serverIPs {
		if utilnet.IsIPv6String(ip) == wantV6 {
			return ip
		}
	}
	framework.Failf("no bgpserver IP for family of egress IP %s in %+v", egressIP, serverIPs)
	return ""
}

func waitForPodsReady(cs clientset.Interface, ns string, podNames ...string) {
	for _, name := range podNames {
		gomega.Eventually(func() error {
			pod, err := cs.CoreV1().Pods(ns).Get(context.TODO(), name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if !podutils.IsPodReady(pod) {
				return fmt.Errorf("pod %s not ready", name)
			}
			return nil
		}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed())
	}
}

func copyAssignmentMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
