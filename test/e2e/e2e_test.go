//go:build e2e
// +build e2e

/*
Copyright 2026.

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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	dexapi "github.com/dexidp/dex/api/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"groeneslot.nl/dex-idp-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "dex-idp-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "dex-idp-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "dex-idp-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "dex-idp-operator-metrics-binding"

const dexNamespace = "dex"

// tenantANamespace and tenantBNamespace host the two managed DexTenant instances used by the
// multi-tenant e2e scenario. Each DexTenant needs its own namespace because Dex's Kubernetes
// storage backend scopes connector visibility to the namespace the Dex Deployment runs in.
const (
	tenantANamespace = "tenant-a-e2e"
	tenantBNamespace = "tenant-b-e2e"
)

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("installing Dex for functional tests")
		installDex()
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("cleaning up the metrics ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("removing Dex")
		uninstallDex()

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("removing any existing metrics ClusterRoleBinding")
			cmd := exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to remove existing ClusterRoleBinding")

			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd = exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=dex-idp-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		It("should reconcile Dex OAuth2 clients into Dex", func() {
			By("creating Dex connection and client resources")
			applyYAML(fmt.Sprintf(dexClientE2EManifest, namespace))

			By("waiting for the DexClient to sync")
			Eventually(dexClientCondition(namespace, "e2e-app", "Synced"), 2*time.Minute, time.Second).
				Should(Equal("True"))

			By("checking the client exists in Dex")
			dexAPI, closeDex := portForwardDex()
			defer closeDex()

			Eventually(func(g Gomega) {
				client, err := dexAPI.GetClient(context.Background(), &dexapi.GetClientReq{Id: "e2e-app"})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(client.GetClient().GetSecret()).To(Equal("initial-secret"))
				g.Expect(client.GetClient().GetRedirectUris()).To(Equal([]string{"https://app.example.com/callback"}))
			}, time.Minute, time.Second).Should(Succeed())

			By("updating mutable Dex client fields")
			patchDexClient(namespace, "e2e-app", `{
				"spec": {
					"redirectURIs": ["https://app.example.com/updated-callback"],
					"name": "Updated E2E App"
				}
			}`)
			Eventually(func(g Gomega) {
				client, err := dexAPI.GetClient(context.Background(), &dexapi.GetClientReq{Id: "e2e-app"})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(client.GetClient().GetName()).To(Equal("Updated E2E App"))
				g.Expect(client.GetClient().GetRedirectUris()).To(Equal([]string{"https://app.example.com/updated-callback"}))
			}, time.Minute, time.Second).Should(Succeed())

			By("rotating the Secret and observing blocked immutable change")
			patchSecret(namespace, "e2e-app-secret", "rotated-secret")
			Eventually(dexClientCondition(namespace, "e2e-app", "Blocked"), time.Minute, time.Second).
				Should(Equal("True"))

			By("allowing recreate for immutable change")
			patchDexClient(namespace, "e2e-app", `{
				"spec": {
					"recreateOnImmutableChange": true
				}
			}`)
			Eventually(dexClientCondition(namespace, "e2e-app", "Synced"), 2*time.Minute, time.Second).
				Should(Equal("True"))
			Eventually(func(g Gomega) {
				client, err := dexAPI.GetClient(context.Background(), &dexapi.GetClientReq{Id: "e2e-app"})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(client.GetClient().GetSecret()).To(Equal("rotated-secret"))
			}, time.Minute, time.Second).Should(Succeed())

			By("deleting the DexClient and verifying Dex cleanup")
			cmd := exec.Command("kubectl", "delete", "dexclient", "e2e-app", "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "dexclient", "e2e-app", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}, time.Minute, time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				_, err := dexAPI.GetClient(context.Background(), &dexapi.GetClientReq{Id: "e2e-app"})
				g.Expect(err).To(HaveOccurred())
				g.Expect(status.Convert(err).Message()).To(ContainSubstring("not found"))
			}, time.Minute, time.Second).Should(Succeed())
		})

		It("should route a multi-tenant DexClient to per-tenant managed Dex instances", func() {
			By("creating two tenant namespaces, connectors, DexTenants, and a multi-tenant DexClient")
			applyYAML(fmt.Sprintf(dexTenantE2EManifest, namespace))
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "ns", tenantANamespace, tenantBNamespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			By("waiting for both DexTenants to become ready")
			Eventually(dexTenantCondition(tenantANamespace, "tenant-a", "Ready"), 3*time.Minute, time.Second).
				Should(Equal("True"))
			Eventually(dexTenantCondition(tenantBNamespace, "tenant-b", "Ready"), 3*time.Minute, time.Second).
				Should(Equal("True"))

			By("waiting for the multi-tenant DexClient to sync onto both tenants")
			Eventually(dexClientCondition(namespace, "multi-app", "Synced"), 2*time.Minute, time.Second).
				Should(Equal("True"))

			By("verifying the client exists in tenant A's Dex with its own generated secret")
			dexA, closeDexA := portForwardService(tenantANamespace, "tenant-a-dex")
			defer closeDexA()
			var secretA string
			Eventually(func(g Gomega) {
				resp, err := dexA.GetClient(context.Background(), &dexapi.GetClientReq{Id: "multi-app"})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(resp.GetClient().GetRedirectUris()).To(Equal([]string{"https://multi-app.example.com/callback"}))
				secretA = resp.GetClient().GetSecret()
				g.Expect(secretA).NotTo(BeEmpty())
			}, time.Minute, time.Second).Should(Succeed())

			By("verifying the client exists in tenant B's Dex with a different generated secret")
			dexB, closeDexB := portForwardService(tenantBNamespace, "tenant-b-dex")
			defer closeDexB()
			Eventually(func(g Gomega) {
				resp, err := dexB.GetClient(context.Background(), &dexapi.GetClientReq{Id: "multi-app"})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(resp.GetClient().GetRedirectUris()).To(Equal([]string{"https://multi-app.example.com/callback"}))
				g.Expect(resp.GetClient().GetSecret()).NotTo(BeEmpty())
				g.Expect(resp.GetClient().GetSecret()).NotTo(Equal(secretA))
			}, time.Minute, time.Second).Should(Succeed())

			By("deleting the DexClient and verifying cleanup from both tenants")
			cmd := exec.Command("kubectl", "delete", "dexclient", "multi-app", "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				_, err := dexA.GetClient(context.Background(), &dexapi.GetClientReq{Id: "multi-app"})
				g.Expect(err).To(HaveOccurred())
			}, time.Minute, time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				_, err := dexB.GetClient(context.Background(), &dexapi.GetClientReq{Id: "multi-app"})
				g.Expect(err).To(HaveOccurred())
			}, time.Minute, time.Second).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})
})

const dexManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: dex
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: dex-config
  namespace: dex
data:
  config.yaml: |
    issuer: http://dex.dex.svc.cluster.local:5556/dex
    storage:
      type: sqlite3
      config:
        file: /tmp/dex.db
    web:
      http: 0.0.0.0:5556
    grpc:
      addr: 0.0.0.0:5557
    connectors:
    - type: mockCallback
      id: mock
      name: Mock
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dex
  namespace: dex
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: dex
  template:
    metadata:
      labels:
        app.kubernetes.io/name: dex
    spec:
      containers:
      - name: dex
        image: ghcr.io/dexidp/dex:v2.43.1
        command:
        - dex
        - serve
        - /etc/dex/config.yaml
        ports:
        - name: http
          containerPort: 5556
        - name: grpc
          containerPort: 5557
        volumeMounts:
        - name: config
          mountPath: /etc/dex
          readOnly: true
      volumes:
      - name: config
        configMap:
          name: dex-config
---
apiVersion: v1
kind: Service
metadata:
  name: dex
  namespace: dex
spec:
  selector:
    app.kubernetes.io/name: dex
  ports:
  - name: http
    port: 5556
    targetPort: http
  - name: grpc
    port: 5557
    targetPort: grpc
`

const dexClientE2EManifest = `apiVersion: v1
kind: Secret
metadata:
  name: e2e-app-secret
  namespace: %[1]s
stringData:
  client-secret: initial-secret
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexConnection
metadata:
  name: dex-e2e
  namespace: %[1]s
spec:
  address: dex.dex.svc.cluster.local:5557
  plaintext: true
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexClient
metadata:
  name: e2e-app
  namespace: %[1]s
spec:
  connectionRef:
    name: dex-e2e
  clientID: e2e-app
  secret:
    valueFrom:
      secretKeyRef:
        name: e2e-app-secret
        key: client-secret
  redirectURIs:
  - https://app.example.com/callback
  name: E2E App
`

const dexTenantE2EManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: tenant-a-e2e
---
apiVersion: v1
kind: Namespace
metadata:
  name: tenant-b-e2e
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexConnector
metadata:
  name: tenant-a-connector
  namespace: tenant-a-e2e
spec:
  connectorID: mock
  type: mockCallback
  displayName: Tenant A Mock
  config: {}
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexConnector
metadata:
  name: tenant-b-connector
  namespace: tenant-b-e2e
spec:
  connectorID: mock
  type: mockCallback
  displayName: Tenant B Mock
  config: {}
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexTenant
metadata:
  name: tenant-a
  namespace: tenant-a-e2e
spec:
  issuer: http://tenant-a-dex.tenant-a-e2e.svc.cluster.local:5556/dex
  connectorRef:
    name: tenant-a-connector
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexTenant
metadata:
  name: tenant-b
  namespace: tenant-b-e2e
spec:
  issuer: http://tenant-b-dex.tenant-b-e2e.svc.cluster.local:5556/dex
  connectorRef:
    name: tenant-b-connector
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexClient
metadata:
  name: multi-app
  namespace: %[1]s
spec:
  tenantRefs:
  - name: tenant-a
    namespace: tenant-a-e2e
  - name: tenant-b
    namespace: tenant-b-e2e
  clientID: multi-app
  redirectURIs:
  - https://multi-app.example.com/callback
  name: Multi Tenant App
`

func installDex() {
	applyYAML(dexManifest)

	By("waiting for Dex deployment")
	cmd := exec.Command("kubectl", "wait", "deployment.apps/dex",
		"--for", "condition=Available",
		"--namespace", dexNamespace,
		"--timeout", "5m",
	)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed waiting for Dex")
}

func uninstallDex() {
	cmd := exec.Command("kubectl", "delete", "ns", dexNamespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

func applyYAML(manifest string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

func patchDexClient(namespace, name, patch string) {
	cmd := exec.Command("kubectl", "patch", "dexclient", name, "-n", namespace, "--type", "merge", "-p", patch)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

func patchSecret(namespace, name, secret string) {
	encodedSecret := base64.StdEncoding.EncodeToString([]byte(secret))
	patch := fmt.Sprintf(`{"data":{"client-secret":"%s"}}`, encodedSecret)
	cmd := exec.Command("kubectl", "patch", "secret", name, "-n", namespace, "--type", "merge", "-p", patch)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

func dexClientCondition(namespace, name, conditionType string) func() string {
	return func() string {
		cmd := exec.Command("kubectl", "get", "dexclient", name, "-n", namespace,
			"-o", fmt.Sprintf("jsonpath={.status.conditions[?(@.type==\"%s\")].status}", conditionType))
		output, err := utils.Run(cmd)
		if err != nil {
			return ""
		}
		return output
	}
}

func dexTenantCondition(namespace, name, conditionType string) func() string {
	return func() string {
		cmd := exec.Command("kubectl", "get", "dextenant", name, "-n", namespace,
			"-o", fmt.Sprintf("jsonpath={.status.conditions[?(@.type==\"%s\")].status}", conditionType))
		output, err := utils.Run(cmd)
		if err != nil {
			return ""
		}
		return output
	}
}

func portForwardDex() (dexapi.DexClient, func()) {
	return portForwardService(dexNamespace, "dex")
}

// portForwardService port-forwards to the gRPC port of the named Dex Service in namespace and
// returns a Dex API client dialed through the forwarded port, along with a cleanup function
// that closes the connection and stops the port-forward.
func portForwardService(namespace, serviceName string) (dexapi.DexClient, func()) {
	port := freeLocalPort()
	ctx, cancel := context.WithCancel(context.Background())

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", namespace,
		"service/"+serviceName, fmt.Sprintf("%d:5557", port))
	cmd.Stderr = &stderr
	err := cmd.Start()
	Expect(err).NotTo(HaveOccurred())

	target := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	Expect(err).NotTo(HaveOccurred())

	dexAPI := dexapi.NewDexClient(conn)
	Eventually(func(g Gomega) {
		_, err := dexAPI.GetVersion(context.Background(), &dexapi.VersionReq{})
		g.Expect(err).NotTo(HaveOccurred(), stderr.String())
	}, time.Minute, time.Second).Should(Succeed())

	return dexAPI, func() {
		Expect(conn.Close()).To(Succeed())
		cancel()
		_ = cmd.Wait()
	}
}

func freeLocalPort() int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	defer listener.Close() //nolint:errcheck

	return listener.Addr().(*net.TCPAddr).Port
}

func run(cmd *exec.Cmd) (string, error) {
	dir, _ := utils.GetProjectDir()
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err != nil {
		return output.String(), fmt.Errorf("%q failed with error %q: %w", command, output.String(), err)
	}

	return output.String(), nil
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
