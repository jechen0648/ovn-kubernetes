package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"

	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	utilnet "k8s.io/utils/net"
)

// Validate the egress firewall policies by applying a policy and verify
// that both explicitly allowed traffic and implicitly denied traffic
// is properly handled as defined in the crd configuration in the test.
var _ = ginkgo.Describe("e2e egress firewall policy validation", feature.EgressFirewall, func() {
	const (
		svcname                string = "egress-firewall-policy"
		egressFirewallYamlFile string = "egress-fw.yml"
		testTimeout            int    = 3
		retryInterval                 = 1 * time.Second
		retryTimeout                  = 30 * time.Second
	)

	type nodeInfo struct {
		name   string
		nodeIP string
	}

	var (
		serverNodeInfo nodeInfo
		denyAllCIDR    string
	)

	waitForEFApplied := func(namespace string) {
		gomega.Eventually(func() bool {
			output, err := e2ekubectl.RunKubectl(namespace, "get", "egressfirewall", "default")
			if err != nil {
				framework.Failf("could not get the egressfirewall default in namespace: %s", namespace)
			}
			return strings.Contains(output, "EgressFirewall Rules applied")
		}, 10*time.Second).Should(gomega.BeTrue(), fmt.Sprintf("expected egress firewall in namespace %s to be successfully applied", namespace))
	}

	applyEF := func(egressFirewallConfig, namespace string) {
		// write the config to a file for application and defer the removal
		if err := os.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressFirewallYamlFile); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()
		// create the CRD config parameters
		applyArgs := []string{
			"apply",
			"--namespace=" + namespace,
			"-f",
			egressFirewallYamlFile,
		}
		framework.Logf("Applying EgressFirewall configuration: %s ", applyArgs)
		// apply the egress firewall configuration
		e2ekubectl.RunKubectlOrDie(namespace, applyArgs...)
		waitForEFApplied(namespace)
	}

	f := wrappedTestFramework(svcname)

	// Determine what mode the CI is running in and get relevant endpoint information for the tests
	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf(
				"Test requires >= 2 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)

		serverNodeInfo = nodeInfo{
			name:   nodes.Items[1].Name,
			nodeIP: ips[1],
		}

		denyAllCIDR = "0.0.0.0/0"
		if IsIPv6Cluster(f.ClientSet) {
			denyAllCIDR = "::/0"
		}
	})

	ginkgo.Context("with external containers", func() {
		const (
			externalContainerName1 = "e2e-egress-fw-external-container1"
			externalContainerName2 = "e2e-egress-fw-external-container2"
		)

		var (
			singleIPMask, subnetMask string
			providerCtx              infraapi.Context
			externalContainer1       infraapi.ExternalContainer
			externalContainer2       infraapi.ExternalContainer
		)

		getExternalContainerIP := func(externalContainer infraapi.ExternalContainer) string {
			if IsIPv6Cluster(f.ClientSet) {
				return externalContainer.GetIPv6()
			}
			return externalContainer.GetIPv4()
		}

		checkConnectivity := func(srcPodName, dstIP string, dstPort string, shouldSucceed bool) {
			testContainer := fmt.Sprintf("%s-container", srcPodName)
			testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
			if shouldSucceed {
				gomega.Eventually(func() bool {
					_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--",
						"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(dstIP, dstPort))
					return err == nil
				}, time.Duration(2*testTimeout)*time.Second).Should(gomega.BeTrue(),
					fmt.Sprintf("expected connection from %s to [%s]:%s to suceed", srcPodName, dstIP, dstPort))
			} else {
				gomega.Consistently(func() bool {
					_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--",
						"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(dstIP, dstPort))
					return err != nil
				}, time.Duration(2*testTimeout)*time.Second).Should(gomega.BeTrue(),
					fmt.Sprintf("expected connection from %s to [%s]:%s to fail", srcPodName, dstIP, dstPort))
			}
		}

		checkExternalContainerConnectivity := func(externalContainer infraapi.ExternalContainer, dstIP string, dstPort int) {
			_, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{
				"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(dstIP, fmt.Sprint(dstPort)),
			})
			if err != nil {
				framework.Failf("Failed to connect from external container %s to %s:%d: %v",
					externalContainer.GetName(), dstIP, dstPort, err)
			}
		}

		// createSrcPodWithRetry creates a pod that can reach the specified destination with a given number of retries.
		// In our e2e tests, a strange behaviour for ipv6 was seen: newly created pod can't reach ipv6 destination.
		// But if the same pod is re-created, everything works.
		// We don't know what causes that behaviour, so given function is a workaround for this issue.
		// It also only historically fails for the first ef test "Should validate the egress firewall policy functionality for allowed IP",
		// so only used there for now.
		createSrcPodWithRetry := func(retries int, reachableDst string, reachablePort string,
			podName, nodeName string, ipCheckInterval, ipCheckTimeout time.Duration, f *framework.Framework) {
			for i := 0; i < retries; i++ {
				createSrcPod(podName, nodeName, ipCheckInterval, ipCheckTimeout, f)
				testContainer := fmt.Sprintf("%s-container", podName)
				testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
				for connectRetry := 0; connectRetry < 5; connectRetry++ {
					_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", podName, testContainerFlag, "--",
						"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(reachableDst, reachablePort))
					if err == nil {
						return
					}
				}
				err := deletePodWithWaitByName(context.TODO(), f.ClientSet, podName, f.Namespace.Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
			framework.Failf("Failed to create pod %s that can reach %s:%s after %d retries", podName, reachableDst, reachablePort, retries)
		}

		ginkgo.BeforeEach(func() {
			providerCtx = infraprovider.Get().NewTestContext()
			primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
			// container 1
			externalContainer1Port := infraprovider.Get().GetExternalContainerPort()
			framework.ExpectNoError(err, "must get unique container port for external container 1")
			externalContainer1Spec := infraapi.ExternalContainer{
				Name:    externalContainerName1,
				Image:   images.AgnHost(),
				Network: primaryProviderNetwork,
				CmdArgs: []string{"netexec", fmt.Sprintf("--http-port=%d", externalContainer1Port)},
				ExtPort: externalContainer1Port,
			}
			externalContainer1, err = providerCtx.CreateExternalContainer(externalContainer1Spec)
			framework.ExpectNoError(err, "must create external container 1")

			// container 2
			externalContainer2Port := infraprovider.Get().GetExternalContainerPort()
			framework.ExpectNoError(err, "must get unique container port for external container 2")
			externalContainer2Spec := infraapi.ExternalContainer{
				Name:    externalContainerName2,
				Image:   images.AgnHost(),
				Network: primaryProviderNetwork,
				CmdArgs: []string{"netexec", fmt.Sprintf("--http-port=%d", externalContainer2Port)},
				ExtPort: externalContainer2Port,
			}
			externalContainer2, err = providerCtx.CreateExternalContainer(externalContainer2Spec)
			framework.ExpectNoError(err, "must create external container 2")

			gomega.Eventually(func() bool {
				_, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer1, []string{
					"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(getExternalContainerIP(externalContainer2), fmt.Sprint(externalContainer2.GetPortStr())),
				})
				if err != nil {
					framework.Logf("Failed to connect to container 2 from container 1: %v", err)
					return false
				}
				_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer2, []string{
					"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(getExternalContainerIP(externalContainer1), fmt.Sprint(externalContainer1.GetPortStr())),
				})
				if err != nil {
					framework.Logf("Failed to connect to container 1 from container 2: %v", err)
					return false
				}
				return true
			}, 10*time.Second, 500*time.Millisecond).Should(gomega.BeTrue(), "expected external containers %s to be connected")

			singleIPMask = "32"
			subnetMask = "24"
			if IsIPv6Cluster(f.ClientSet) {
				singleIPMask = "128"
				subnetMask = "64"
			}
		})

		ginkgo.It("Should validate the egress firewall policy functionality for allowed IP", func() {
			srcPodName := "e2e-egress-fw-src-pod"
			// create the pod that will be used as the source for the connectivity test
			createSrcPodWithRetry(3, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(),
				srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)

			// egress firewall crd yaml configuration
			var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: %s/%s
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, getExternalContainerIP(externalContainer1), singleIPMask, denyAllCIDR)
			applyEF(egressFirewallConfig, f.Namespace.Name)

			// Verify the remote host/port as explicitly allowed by the firewall policy is reachable
			ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed host %s is permitted as defined "+
				"by the external firewall policy", getExternalContainerIP(externalContainer1)))
			checkConnectivity(srcPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), true)

			// Verify the remote host/port as implicitly denied by the firewall policy is not reachable
			ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied host %s is not permitted as defined "+
				"by the external firewall policy", getExternalContainerIP(externalContainer2)))
			checkConnectivity(srcPodName, getExternalContainerIP(externalContainer2), externalContainer2.GetPortStr(), false)
		})

		ginkgo.It("Should validate the egress firewall policy functionality for allowed CIDR and port", func() {
			srcPodName := "e2e-egress-fw-src-pod"
			// egress firewall crd yaml configuration
			var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: %s/%s
    ports:
      - protocol: TCP
        port: %s
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, getExternalContainerIP(externalContainer1), subnetMask, externalContainer1.GetPortStr(), denyAllCIDR)
			applyEF(egressFirewallConfig, f.Namespace.Name)

			// create the pod that will be used as the source for the connectivity test
			createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)

			// Verify the remote host/port as explicitly allowed by the firewall policy is reachable
			ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed port on host %s is permitted as "+
				"defined by the external firewall policy", getExternalContainerIP(externalContainer1)))
			checkConnectivity(srcPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), true)

			// Verify the remote host/port as implicitly denied by the firewall policy is not reachable
			ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied port on host %s is not permitted as "+
				"defined by the external firewall policy", getExternalContainerIP(externalContainer2)))
			checkConnectivity(srcPodName, getExternalContainerIP(externalContainer2), externalContainer2.GetPortStr(), false)
		})

		ginkgo.It("Should validate the egress firewall allows inbound connections", func() {
			// 1. Create nodePort service and external container
			// 2. Check connectivity works both ways
			// 3. Apply deny-all egress firewall
			// 4. Check only inbound traffic is allowed

			efPodName := "e2e-egress-fw-pod"
			var efPodPort uint16 = 1234
			serviceName := "service-for-pods"
			var servicePort uint16 = 31234

			ginkgo.By("Creating the egress firewall pod")
			// 1. create nodePort service and external container
			endpointsSelector := map[string]string{"servicebackend": "true"}
			_, err := createPod(f, efPodName, serverNodeInfo.name, f.Namespace.Name,
				[]string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", efPodPort)}, endpointsSelector)
			if err != nil {
				framework.Failf("Failed to create pod %s: %v", efPodName, err)
			}

			ginkgo.By("Creating the nodePort service")
			_, err = createServiceForPodsWithLabel(f, f.Namespace.Name, servicePort, efPodPort, "NodePort", endpointsSelector)
			framework.ExpectNoError(err, fmt.Sprintf("unable to create nodePort service, err: %v", err))

			ginkgo.By("Waiting for the endpoints to pop up")
			err = framework.WaitForServiceEndpointsNum(context.TODO(), f.ClientSet, f.Namespace.Name, serviceName, 1, time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName, f.Namespace.Name)

			// 2. Check connectivity works both ways
			// pod -> external container should work
			ginkgo.By(fmt.Sprintf("Verifying connectivity from pod %s to external container [%s]:%s",
				efPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr()))
			checkConnectivity(efPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), true)

			// external container -> nodePort svc should work
			svc, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to fetch service: %s in namespace %s", serviceName, f.Namespace.Name)

			nodeIP := serverNodeInfo.nodeIP
			ginkgo.By(fmt.Sprintf("Verifying connectivity from external container %s to nodePort svc [%s]:%d",
				getExternalContainerIP(externalContainer1), nodeIP, svc.Spec.Ports[0].NodePort))
			checkExternalContainerConnectivity(externalContainer1, nodeIP, int(svc.Spec.Ports[0].NodePort))

			// 3. Apply deny-all egress firewall and wait for it to be applied
			var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyAllCIDR)
			applyEF(egressFirewallConfig, f.Namespace.Name)

			// 4. Check that only inbound traffic is allowed
			// pod -> external container should be blocked
			ginkgo.By(fmt.Sprintf("Verifying connectivity from pod %s to external container [%s]:%s is blocked",
				efPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr()))
			checkConnectivity(efPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), false)

			// external container -> nodePort svc should work
			ginkgo.By(fmt.Sprintf("Verifying connectivity from external container %s to nodePort svc [%s]:%d",
				getExternalContainerIP(externalContainer1), nodeIP, svc.Spec.Ports[0].NodePort))
			checkExternalContainerConnectivity(externalContainer1, nodeIP, int(svc.Spec.Ports[0].NodePort))
		})

		ginkgo.It("Should validate the egress firewall doesn't affect internal connections", func() {
			srcPodName := "e2e-egress-fw-src-pod"
			dstPodName := "e2e-egress-fw-dst-pod"
			dstPort := "1234"
			// egress firewall crd yaml configuration
			var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyAllCIDR)
			applyEF(egressFirewallConfig, f.Namespace.Name)

			// create the pod that will be used as the source for the connectivity test
			createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)

			// create dst pod
			dstPod, err := createPod(f, dstPodName, serverNodeInfo.name, f.Namespace.Name,
				[]string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", dstPort)}, nil)
			if err != nil {
				framework.Failf("Failed to create dst pod %s: %v", dstPodName, err)
			}
			dstPodIP := dstPod.Status.PodIP

			ginkgo.By(fmt.Sprintf("Verifying connectivity to an internal pod %s is permitted", dstPodName))
			checkConnectivity(srcPodName, dstPodIP, dstPort, true)

			ginkgo.By(fmt.Sprintf("Verifying connectivity to an external host %s is not permitted as defined "+
				"by the external firewall policy", getExternalContainerIP(externalContainer1)))
			checkConnectivity(srcPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), false)
		})

		ginkgo.It("Should validate that egressfirewall supports DNS name in caps", func() {
			// egress firewall crd yaml configuration
			var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      dnsName: WWW.TEST.COM
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyAllCIDR)
			applyEF(egressFirewallConfig, f.Namespace.Name)
			framework.Logf("Deleting EgressFirewall in namespace %s", f.Namespace.Name)
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "egressfirewall", "default")
		})
	})

	table.DescribeTable("Should validate the egress firewall policy functionality against cluster nodes by using node selector",
		func(chaosTesting bool) {
			if chaosTesting {
				// apply egressfirewall with many dns names, then delete and check that the next node-selector egress firewall
				// is handled correctly.
				// Using node selector is the best way to check internal egress firewall locking, as node event handler
				// iterates over all existing egress firewalls.
				var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      dnsName: www.test1.com
  - type: Allow
    to:
      dnsName: www.test2.com
  - type: Allow
    to:
      dnsName: www.test3.com
  - type: Allow
    to:
      dnsName: www.test4.com
  - type: Allow
    to:
      dnsName: www.test5.com
  - type: Allow
    to:
      dnsName: www.test6.com
  - type: Allow
    to:
      dnsName: www.test7.com
  - type: Allow
    to:
      dnsName: www.test8.com
  - type: Allow
    to:
      dnsName: www.test9.com
  - type: Allow
    to:
      dnsName: www.test10.com
  - type: Allow
    to:
      dnsName: www.test11.com
  - type: Allow
    to:
      dnsName: www.test12.com
    ports:
      - protocol: TCP
        port: 80
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyAllCIDR)
				applyEF(egressFirewallConfig, f.Namespace.Name)
				framework.Logf("Deleting EgressFirewall in namespace %s", f.Namespace.Name)
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "egressfirewall", "default")
			}

			srcPodName := "e2e-egress-fw-src-pod"
			testContainer := fmt.Sprintf("%s-container", srcPodName)
			testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
			// use random labels in case test runs again since it's a pain to remove the label from the node
			labelMatch := randStr(5)
			// egress firewall crd yaml configuration
			var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      nodeSelector:
        matchLabels:
          %s: %s
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, f.Namespace.Name, labelMatch, denyAllCIDR)
			framework.Logf("Egress Firewall CR generated: %s", egressFirewallConfig)

			applyEF(egressFirewallConfig, f.Namespace.Name)

			// create the pod that will be used as the source for the connectivity test
			createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)
			// create host networked pod
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)

			if len(nodes.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}
			ginkgo.By("Creating host network pods on each node")
			// get random port in case the test retries and port is already in use on host node
			rand.Seed(time.Now().UnixNano())
			min := 9900
			max := 9999
			hostNetworkPort := rand.Intn(max-min+1) + min
			framework.Logf("Random host networked port chosen: %d", hostNetworkPort)
			for _, node := range nodes.Items {
				// this creates a udp / http netexec listener which is able to receive the "hostname"
				// command. We use this to validate that each endpoint is received at least once
				args := []string{
					"netexec",
					fmt.Sprintf("--http-port=%d", hostNetworkPort),
					fmt.Sprintf("--udp-port=%d", hostNetworkPort),
				}

				// create host networked Pods
				_, err := createPod(f, node.Name+"-hostnet-ep", node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
					p.Spec.Containers[0].Args = args
					p.Spec.HostNetwork = true
				})

				framework.ExpectNoError(err)
			}

			ginkgo.By("Selecting additional IP addresses for serverNode on which source pod lives (networking routing to secondaryIP address on other nodes is harder to achieve)")
			// add new secondary IP from node subnet to the node where the source pod lives on,
			// if the cluster is v6 add an ipv6 address
			toCurlSecondaryNodeIPAddresses := sets.NewString()
			// node2ndaryIPs holds the nodeName as the key and the value is
			// a map with ipFamily(v4 or v6) as the key and the secondaryIP as the value
			node2ndaryIPs := make(map[string]map[int]string)
			var newIP string
			if node2ndaryIPs[serverNodeInfo.name] == nil {
				node2ndaryIPs[serverNodeInfo.name] = make(map[int]string)
			}
			if utilnet.IsIPv6String(e2enode.GetAddresses(&nodes.Items[1], v1.NodeInternalIP)[0]) {
				newIP = "fc00:f853:ccd:e794::" + strconv.Itoa(12)
				framework.Logf("Secondary nodeIP %s for node %s", serverNodeInfo.name, newIP)
				node2ndaryIPs[serverNodeInfo.name][6] = newIP
			} else {
				newIP = "172.18.1." + strconv.Itoa(13)
				framework.Logf("Secondary nodeIP %s for node %s", serverNodeInfo.name, newIP)
				node2ndaryIPs[serverNodeInfo.name][4] = newIP
			}

			ginkgo.By("Adding additional IP addresses to node on which source pod lives")
			for nodeName, ipFamilies := range node2ndaryIPs {
				for _, ip := range ipFamilies {
					// manually add the a secondary IP to each node
					framework.Logf("Adding IP %s to node %s", ip, nodeName)
					_, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{
						"ip", "addr", "add", ip, "dev", deploymentconfig.Get().PrimaryInterfaceName(),
					})
					if err != nil && !strings.Contains(err.Error(), "Address already assigned") {
						framework.Failf("failed to add new IP address %s to node %s: %v", ip, nodeName, err)
					}
					ginkgo.DeferCleanup(func() error {
						_, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "addr", "delete", ip, "dev", deploymentconfig.Get().PrimaryInterfaceName()})
						return err
					})
					toCurlSecondaryNodeIPAddresses.Insert(ip)
				}
			}

			ginkgo.By("Should NOT be able to reach each host networked pod via node selector")
			hostNetworkPortStr := fmt.Sprint(hostNetworkPort)
			for _, node := range nodes.Items {
				path := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(node.Status.Addresses[0].Address, hostNetworkPortStr))
				_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
				if err == nil {
					framework.Failf("Was able to curl node %s from container %s on node %s with no allow rule for egress firewall", node.Name, srcPodName, serverNodeInfo.name)
				}
			}

			ginkgo.By("Should NOT be able to reach each secondary hostIP via node selector")
			for _, address := range toCurlSecondaryNodeIPAddresses.List() {
				if !IsIPv6Cluster(f.ClientSet) && utilnet.IsIPv6String(address) || IsIPv6Cluster(f.ClientSet) && !utilnet.IsIPv6String(address) {
					continue
				}
				path := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(address, hostNetworkPortStr))
				_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
				if err == nil {
					framework.Failf("Was able to curl node %s from container %s on nodeIP %s with no allow rule for egress firewall", address, srcPodName, serverNodeInfo.name)
				}
			}

			ginkgo.By("Apply label to nodes " + f.Namespace.Name + ":" + labelMatch)
			patch := struct {
				Metadata map[string]interface{} `json:"metadata"`
			}{
				Metadata: map[string]interface{}{
					"labels": map[string]string{f.Namespace.Name: labelMatch},
				},
			}
			for _, node := range nodes.Items {
				patchData, err := json.Marshal(&patch)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				f.ClientSet.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.MergePatchType, patchData, metav1.PatchOptions{})
			}

			ginkgo.By("Should be able to reach each host networked pod via node selector")
			for _, node := range nodes.Items {
				path := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(node.Status.Addresses[0].Address, hostNetworkPortStr))
				_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
				if err != nil {
					framework.Failf("Failed to curl node %s from container %s on node %s: %v", node.Name, srcPodName, serverNodeInfo.name, err)
				}
			}

			ginkgo.By("Should be able to reach secondary hostIP via node selector")
			for _, address := range toCurlSecondaryNodeIPAddresses.List() {
				if !IsIPv6Cluster(f.ClientSet) && utilnet.IsIPv6String(address) || IsIPv6Cluster(f.ClientSet) && !utilnet.IsIPv6String(address) {
					continue
				}
				path := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(address, hostNetworkPortStr))
				_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
				if err != nil {
					framework.Failf("Failed to curl node %s from container %s on nodeIP %s", address, srcPodName, serverNodeInfo.name)
				}
			}
		},
		table.Entry("", false),
		table.Entry("with chaos testing using many dnsNames", true),
	)
})
