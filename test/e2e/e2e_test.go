/*
Copyright 2025.

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
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/PhilCANDIDO/mariadb-ha-operator/api/v1alpha1"
	"github.com/PhilCANDIDO/mariadb-ha-operator/test/utils"
)

const (
	namespace             = "mariadb-ha-operator-system"
	testNamespace         = "mariadb-ha-test"
	operatorDeployTimeout = 5 * time.Minute
	testTimeout           = 10 * time.Minute
	pollingInterval       = 5 * time.Second
)

var _ = Describe("MariaDB HA Operator", Ordered, func() {
	var k8sClient client.Client
	var ctx context.Context
	var cancel context.CancelFunc
	var projectImage = "example.com/mariadb-ha-operator:v0.0.1"

	BeforeAll(func() {
		By("installing prometheus operator")
		Expect(utils.InstallPrometheusOperator()).To(Succeed())

		By("installing the cert-manager")
		Expect(utils.InstallCertManager()).To(Succeed())

		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, _ = utils.Run(cmd)

		By("creating test namespace")
		cmd = exec.Command("kubectl", "create", "ns", testNamespace)
		_, _ = utils.Run(cmd)

		By("building the manager(Operator) image")
		cmd = exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())

		By("loading the manager(Operator) image on Kind")
		err = utils.LoadImageToKindClusterWithName(projectImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())

		// Wait for the controller manager to be ready
		By("waiting for the controller manager to be ready")
		Eventually(func() error {
			cmd = exec.Command("kubectl", "get", "deployment", "mariadb-ha-operator-controller-manager", "-n", namespace, "-o", "jsonpath={.status.readyReplicas}")
			output, err := utils.Run(cmd)
			if err != nil {
				return err
			}
			if string(output) != "1" {
				return fmt.Errorf("controller manager not ready")
			}
			return nil
		}, operatorDeployTimeout, pollingInterval).Should(Succeed())

		// Initialize the k8s client
		ctx, cancel = context.WithCancel(context.Background())
		cfg, err := utils.GetKubeConfig()
		Expect(err).NotTo(HaveOccurred())

		k8sClient, err = client.New(cfg, client.Options{})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient).NotTo(BeNil())

		// Prepare required secrets for MariaDB
		By("creating required secrets for MariaDB")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mariadb-credentials",
				Namespace: testNamespace,
			},
			Type: corev1.SecretTypeOpaque,
			StringData: map[string]string{
				"MARIADB_ROOT_PASSWORD":        "rootpassword",
				"MARIADB_USER":                 "app_user",
				"MARIADB_PASSWORD":             "password",
				"MARIADB_DATABASE":             "testdb",
				"MARIADB_REPLICATION_PASSWORD": "replpassword",
			},
		}
		err = k8sClient.Create(ctx, secret)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		cancel()

		By("cleaning up test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace)
		_, _ = utils.Run(cmd)

		By("uninstalling the operator")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling the Prometheus manager bundle")
		utils.UninstallPrometheusOperator()

		By("uninstalling the cert-manager bundle")
		utils.UninstallCertManager()

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	Context("when deploying a MariaDB HA cluster", func() {
		var crName string

		BeforeEach(func() {
			crName = "test-mariadbha"

			// Make sure any previous CR is deleted
			mariaDBHA := &databasev1alpha1.MariaDBHA{
				ObjectMeta: metav1.ObjectMeta{
					Name:      crName,
					Namespace: testNamespace,
				},
			}
			err := k8sClient.Delete(ctx, mariaDBHA)
			if err != nil && !errors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			// Wait for resources to be cleaned up if they existed
			time.Sleep(5 * time.Second)
		})

		It("should successfully deploy and create a functioning HA cluster", func() {
			By("creating a MariaDBHA custom resource")
			mariaDBHA := &databasev1alpha1.MariaDBHA{
				ObjectMeta: metav1.ObjectMeta{
					Name:      crName,
					Namespace: testNamespace,
				},
				Spec: databasev1alpha1.MariaDBHASpec{
					Instances: databasev1alpha1.InstancesConfig{
						CommonConfig: databasev1alpha1.MariaDBConfig{
							Version: "11.5",
							Image:   "mariadb:11.5",
							ConfigurationParameters: map[string]string{
								"max_connections":         "150",
								"innodb_buffer_pool_size": "256M",
								"character-set-server":    "utf8mb4",
								"collation-server":        "utf8mb4_unicode_ci",
							},
							UserCredentialsSecret: &databasev1alpha1.SecretReference{
								Name: "mariadb-credentials",
							},
						},
						Primary: databasev1alpha1.InstanceConfig{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
							Storage: databasev1alpha1.StorageConfig{
								Size: "1Gi",
								AccessModes: []corev1.PersistentVolumeAccessMode{
									corev1.ReadWriteOnce,
								},
							},
							Config: databasev1alpha1.MariaDBConfig{
								ConfigurationParameters: map[string]string{
									"innodb_flush_log_at_trx_commit": "1",
									"sync_binlog":                    "1",
								},
							},
						},
						Secondary: databasev1alpha1.InstanceConfig{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
							Storage: databasev1alpha1.StorageConfig{
								Size: "1Gi",
								AccessModes: []corev1.PersistentVolumeAccessMode{
									corev1.ReadWriteOnce,
								},
							},
							Config: databasev1alpha1.MariaDBConfig{
								ConfigurationParameters: map[string]string{
									"innodb_flush_log_at_trx_commit": "2",
									"sync_binlog":                    "0",
									"read_only":                      "ON",
								},
							},
						},
					},
					Replication: databasev1alpha1.ReplicationConfig{
						Mode:              "async",
						User:              "replicator",
						AutomaticFailover: boolPtr(true),
						MaxLagSeconds:     int32Ptr(30),
						ReconnectRetries:  int32Ptr(3),
					},
					Failover: databasev1alpha1.FailoverConfig{
						FailureDetection: databasev1alpha1.FailureDetectionConfig{
							HealthCheckIntervalSeconds: int32Ptr(5),
							FailureThresholdCount:      int32Ptr(3),
							TimeoutSeconds:             int32Ptr(2),
							HealthChecks: []string{
								"tcpConnection",
								"sqlQuery",
							},
						},
						PromotionStrategy:      "safe",
						MinimumIntervalSeconds: int32Ptr(300),
						AutomaticFailback:      boolPtr(true),
						ReadOnlyMode:           boolPtr(false),
					},
					Monitoring: databasev1alpha1.MonitoringConfig{
						Enabled:            boolPtr(true),
						PrometheusExporter: boolPtr(true),
					},
				},
			}

			err := k8sClient.Create(ctx, mariaDBHA)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the MariaDBHA status is updated to Running")
			Eventually(func() string {
				updatedMariaDBHA := &databasev1alpha1.MariaDBHA{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: testNamespace}, updatedMariaDBHA)
				if err != nil {
					return ""
				}
				return updatedMariaDBHA.Status.Phase
			}, testTimeout, pollingInterval).Should(Equal("Running"))

			By("verifying the StatefulSets are created and running")
			Eventually(func() error {
				// Check primary StatefulSet
				cmd := exec.Command("kubectl", "get", "statefulset", fmt.Sprintf("%s-primary", crName), "-n", testNamespace)
				output, err := utils.Run(cmd)
				if err != nil {
					return err
				}
				if !strings.Contains(string(output), crName) {
					return fmt.Errorf("primary StatefulSet not found")
				}

				// Check secondary StatefulSet
				cmd = exec.Command("kubectl", "get", "statefulset", fmt.Sprintf("%s-secondary", crName), "-n", testNamespace)
				output, err = utils.Run(cmd)
				if err != nil {
					return err
				}
				if !strings.Contains(string(output), crName) {
					return fmt.Errorf("secondary StatefulSet not found")
				}
				return nil
			}, testTimeout, pollingInterval).Should(Succeed())

			By("verifying the Services are created")
			Eventually(func() error {
				// Check main service
				cmd := exec.Command("kubectl", "get", "service", fmt.Sprintf("%s-mariadb", crName), "-n", testNamespace)
				output, err := utils.Run(cmd)
				if err != nil {
					return err
				}
				if !strings.Contains(string(output), crName) {
					return fmt.Errorf("main service not found")
				}

				// Check read-only service
				cmd = exec.Command("kubectl", "get", "service", fmt.Sprintf("%s-mariadb-ro", crName), "-n", testNamespace)
				output, err = utils.Run(cmd)
				if err != nil {
					return err
				}
				if !strings.Contains(string(output), crName) {
					return fmt.Errorf("read-only service not found")
				}
				return nil
			}, testTimeout, pollingInterval).Should(Succeed())

			By("verifying the replication is configured correctly")
			Eventually(func() bool {
				updatedMariaDBHA := &databasev1alpha1.MariaDBHA{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: testNamespace}, updatedMariaDBHA)
				if err != nil {
					return false
				}

				for _, condition := range updatedMariaDBHA.Status.Conditions {
					if condition.Type == "ReplicationActive" && condition.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, testTimeout, pollingInterval).Should(BeTrue())

			By("verifying the pods are running")
			Eventually(func() error {
				// Check primary pod
				cmd := exec.Command("kubectl", "get", "pod", fmt.Sprintf("%s-primary-0", crName), "-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				if err != nil {
					return err
				}
				if string(output) != "Running" {
					return fmt.Errorf("primary pod not running, current status: %s", string(output))
				}

				// Check secondary pod
				cmd = exec.Command("kubectl", "get", "pod", fmt.Sprintf("%s-secondary-0", crName), "-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err = utils.Run(cmd)
				if err != nil {
					return err
				}
				if string(output) != "Running" {
					return fmt.Errorf("secondary pod not running, current status: %s", string(output))
				}
				return nil
			}, testTimeout, pollingInterval).Should(Succeed())

			By("validating the database connection works")
			Eventually(func() error {
				// Create a temporary pod to connect to the database
				cmd := exec.Command("kubectl", "run", "mysql-client",
					"--image=mariadb:11.5",
					"--restart=Never",
					"--namespace", testNamespace,
					"--command", "--", "sleep", "3600")
				_, err := utils.Run(cmd)
				if err != nil {
					return err
				}

				// Wait for the client pod to be ready
				time.Sleep(5 * time.Second)

				// Try to connect to the MariaDB primary
				cmd = exec.Command("kubectl", "exec", "mysql-client", "-n", testNamespace, "--",
					"mysql",
					"-h", fmt.Sprintf("%s-mariadb", crName),
					"-uroot",
					"-prootpassword",
					"-e", "SELECT 1 as test")
				output, err := utils.Run(cmd)
				if err != nil {
					return err
				}
				if !strings.Contains(string(output), "test") {
					return fmt.Errorf("failed to connect to the database, output: %s", string(output))
				}
				return nil
			}, testTimeout, pollingInterval).Should(Succeed())

			By("cleaning up test resources")
			// Delete the MySQL client pod
			cmd := exec.Command("kubectl", "delete", "pod", "mysql-client", "-n", testNamespace)
			_, _ = utils.Run(cmd)

			// Delete the MariaDBHA CR
			Expect(k8sClient.Delete(ctx, mariaDBHA)).To(Succeed())
		})

		It("should handle failure and recovery properly", func() {
			By("creating a MariaDBHA custom resource")
			mariaDBHA := &databasev1alpha1.MariaDBHA{
				ObjectMeta: metav1.ObjectMeta{
					Name:      crName,
					Namespace: testNamespace,
				},
				Spec: databasev1alpha1.MariaDBHASpec{
					Instances: databasev1alpha1.InstancesConfig{
						CommonConfig: databasev1alpha1.MariaDBConfig{
							Version: "11.5",
							Image:   "mariadb:11.5",
							ConfigurationParameters: map[string]string{
								"max_connections":         "150",
								"innodb_buffer_pool_size": "256M",
							},
							UserCredentialsSecret: &databasev1alpha1.SecretReference{
								Name: "mariadb-credentials",
							},
						},
						Primary: databasev1alpha1.InstanceConfig{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
							Storage: databasev1alpha1.StorageConfig{
								Size: "1Gi",
							},
						},
						Secondary: databasev1alpha1.InstanceConfig{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
							Storage: databasev1alpha1.StorageConfig{
								Size: "1Gi",
							},
						},
					},
					Replication: databasev1alpha1.ReplicationConfig{
						Mode:              "async",
						AutomaticFailover: boolPtr(true),
						// Set shorter timeouts for testing
						MaxLagSeconds:    int32Ptr(10),
						ReconnectRetries: int32Ptr(2),
					},
					Failover: databasev1alpha1.FailoverConfig{
						FailureDetection: databasev1alpha1.FailureDetectionConfig{
							// Set shorter timeouts for testing
							HealthCheckIntervalSeconds: int32Ptr(2),
							FailureThresholdCount:      int32Ptr(2),
							TimeoutSeconds:             int32Ptr(1),
						},
						PromotionStrategy:      "immediate",  // Use immediate for faster testing
						MinimumIntervalSeconds: int32Ptr(60), // Shorter interval for testing
						AutomaticFailback:      boolPtr(true),
					},
				},
			}

			err := k8sClient.Create(ctx, mariaDBHA)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the cluster to be in Running state")
			Eventually(func() string {
				updatedMariaDBHA := &databasev1alpha1.MariaDBHA{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: testNamespace}, updatedMariaDBHA)
				if err != nil {
					return ""
				}
				return updatedMariaDBHA.Status.Phase
			}, testTimeout, pollingInterval).Should(Equal("Running"))

			By("simulating a primary failure by deleting the primary pod")
			cmd := exec.Command("kubectl", "delete", "pod", fmt.Sprintf("%s-primary-0", crName), "-n", testNamespace, "--force", "--grace-period=0")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the controller initiates failover")
			Eventually(func() string {
				updatedMariaDBHA := &databasev1alpha1.MariaDBHA{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: testNamespace}, updatedMariaDBHA)
				if err != nil {
					return ""
				}
				return updatedMariaDBHA.Status.Phase
			}, testTimeout, pollingInterval).Should(Or(Equal("Failover"), Equal("Recovering")))

			By("verifying that the cluster eventually returns to Running state after failover")
			Eventually(func() string {
				updatedMariaDBHA := &databasev1alpha1.MariaDBHA{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: testNamespace}, updatedMariaDBHA)
				if err != nil {
					return ""
				}
				return updatedMariaDBHA.Status.Phase
			}, 2*testTimeout, pollingInterval).Should(Equal("Running"))

			By("verifying that failover was recorded in the status")
			updatedMariaDBHA := &databasev1alpha1.MariaDBHA{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: testNamespace}, updatedMariaDBHA)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedMariaDBHA.Status.LastFailoverTime).NotTo(BeNil())
			Expect(updatedMariaDBHA.Status.FailoverHistory).NotTo(BeEmpty())

			By("cleaning up test resources")
			Expect(k8sClient.Delete(ctx, mariaDBHA)).To(Succeed())
		})
	})
})

// Helper functions
func boolPtr(b bool) *bool {
	return &b
}

func int32Ptr(i int32) *int32 {
	return &i
}
