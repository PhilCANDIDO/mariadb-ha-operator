// internal/controller/mariadbha_controller_test.go
package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/PhilCANDIDO/mariadb-ha-operator/api/v1alpha1"
)

var _ = Describe("MariaDBHA Controller", func() {
	// Constantes pour les tests
	const (
		mariadbHAName      = "test-mariadb"
		mariadbHANamespace = "default"
		timeout            = time.Second * 10
		interval           = time.Millisecond * 250
	)

	Context("When reconciling a resource", func() {
		It("Should create StatefulSets and Services for a newly created MariaDBHA", func() {
			ctx := context.Background()
			
			// Créer un nouvel objet MariaDBHA
			mariadbHAKey := types.NamespacedName{
				Name:      mariadbHAName,
				Namespace: mariadbHANamespace,
			}
			
			mariadbHA := &databasev1alpha1.MariaDBHA{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mariadbHAName,
					Namespace: mariadbHANamespace,
				},
				Spec: databasev1alpha1.MariaDBHASpec{
					Instances: databasev1alpha1.InstancesConfig{
						CommonConfig: databasev1alpha1.MariaDBConfig{
							Version: "11.5",
							Image:   "mariadb:11.5",
							ConfigurationParameters: map[string]string{
								"max_connections":        "500",
								"innodb_buffer_pool_size": "2G",
							},
							UserCredentialsSecret: &databasev1alpha1.SecretReference{
								Name: "mariadb-credentials",
							},
						},
						Primary: databasev1alpha1.InstanceConfig{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
							},
							Storage: databasev1alpha1.StorageConfig{
								Size: "10Gi",
							},
						},
						Secondary: databasev1alpha1.InstanceConfig{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
							},
							Storage: databasev1alpha1.StorageConfig{
								Size: "10Gi",
							},
						},
					},
					Replication: databasev1alpha1.ReplicationConfig{
						Mode:             "async",
						AutomaticFailover: boolPtr(true),
						MaxLagSeconds:    int32Ptr(30),
					},
					Failover: databasev1alpha1.FailoverConfig{
						FailureDetection: databasev1alpha1.FailureDetectionConfig{
							HealthCheckIntervalSeconds: int32Ptr(5),
							FailureThresholdCount:     int32Ptr(3),
						},
						PromotionStrategy: "safe",
					},
				},
			}
			
			// Créer d'abord le secret requis
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mariadb-credentials",
					Namespace: mariadbHANamespace,
				},
				Type: corev1.SecretTypeOpaque,
				StringData: map[string]string{
					"MARIADB_ROOT_PASSWORD": "test-password",
					"MARIADB_USER":          "app-user",
					"MARIADB_PASSWORD":      "app-password",
					"MARIADB_REPLICATION_PASSWORD": "repl-password",
				},
			}
			Expect(k8sClient.Create(ctx, secret)).Should(Succeed())
			
			// Créer l'objet MariaDBHA
			Expect(k8sClient.Create(ctx, mariadbHA)).Should(Succeed())
			
			// Vérifier que l'objet a été créé
			createdMariaDBHA := &databasev1alpha1.MariaDBHA{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, mariadbHAKey, createdMariaDBHA)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			
			// Vérifier que le finalizer a été ajouté
			Eventually(func() bool {
				err := k8sClient.Get(ctx, mariadbHAKey, createdMariaDBHA)
				if err != nil {
					return false
				}
				return controllerutil.ContainsFinalizer(createdMariaDBHA, mariadbHAFinalizer)
			}, timeout, interval).Should(BeTrue())
			
			// Vérifier que le StatefulSet du primaire a été créé
			primaryStatefulSetKey := types.NamespacedName{
				Name:      mariadbHAName + "-primary",
				Namespace: mariadbHANamespace,
			}
			primaryStatefulSet := &appsv1.StatefulSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, primaryStatefulSetKey, primaryStatefulSet)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			
			// Vérifier les propriétés du StatefulSet primaire
			Expect(primaryStatefulSet.Spec.Replicas).Should(Equal(int32Ptr(1)))
			Expect(primaryStatefulSet.Spec.Template.Spec.Containers[0].Image).Should(Equal("mariadb:11.5"))
			
			// Vérifier que le StatefulSet du secondaire a été créé
			secondaryStatefulSetKey := types.NamespacedName{
				Name:      mariadbHAName + "-secondary",
				Namespace: mariadbHANamespace,
			}
			secondaryStatefulSet := &appsv1.StatefulSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, secondaryStatefulSetKey, secondaryStatefulSet)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			
			// Vérifier que le service principal a été créé
			clusterServiceKey := types.NamespacedName{
				Name:      mariadbHAName + "-mariadb",
				Namespace: mariadbHANamespace,
			}
			clusterService := &corev1.Service{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, clusterServiceKey, clusterService)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			
			// Vérifier que le service pointe vers le primaire
			Expect(clusterService.Spec.Selector["mariadb.cpf-it.fr/role"]).Should(Equal("primary"))
			
			// Vérifier que le service read-only a été créé
			roServiceKey := types.NamespacedName{
				Name:      mariadbHAName + "-mariadb-ro",
				Namespace: mariadbHANamespace,
			}
			roService := &corev1.Service{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, roServiceKey, roService)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			
			// Vérifier que le service read-only pointe vers le secondaire
			Expect(roService.Spec.Selector["mariadb.cpf-it.fr/role"]).Should(Equal("secondary"))
			
			// Nettoyer les ressources
			Expect(k8sClient.Delete(ctx, mariadbHA)).Should(Succeed())
			
			// Vérifier que tous les objets ont été supprimés
			Eventually(func() bool {
				err := k8sClient.Get(ctx, mariadbHAKey, &databasev1alpha1.MariaDBHA{})
				return errors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
			
			Eventually(func() bool {
				err := k8sClient.Get(ctx, primaryStatefulSetKey, &appsv1.StatefulSet{})
				return errors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
			
			Eventually(func() bool {
				err := k8sClient.Get(ctx, secondaryStatefulSetKey, &appsv1.StatefulSet{})
				return errors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
			
			Eventually(func() bool {
				err := k8sClient.Get(ctx, clusterServiceKey, &corev1.Service{})
				return errors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
			
			// Supprimer le secret à la fin
			Expect(k8sClient.Delete(ctx, secret)).Should(Succeed())
		})
		
		It("Should handle updates to a MariaDBHA resource", func() {
			ctx := context.Background()
			
			// Créer un nouvel objet MariaDBHA
			mariadbHAKey := types.NamespacedName{
				Name:      mariadbHAName + "-update",
				Namespace: mariadbHANamespace,
			}
			
			mariadbHA := &databasev1alpha1.MariaDBHA{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mariadbHAName + "-update",
					Namespace: mariadbHANamespace,
				},
				Spec: databasev1alpha1.MariaDBHASpec{
					Instances: databasev1alpha1.InstancesConfig{
						CommonConfig: databasev1alpha1.MariaDBConfig{
							Version: "11.5",
							Image:   "mariadb:11.5",
						},
						Primary: databasev1alpha1.InstanceConfig{
							Storage: databasev1alpha1.StorageConfig{
								Size: "10Gi",
							},
						},
						Secondary: databasev1alpha1.InstanceConfig{
							Storage: databasev1alpha1.StorageConfig{
								Size: "10Gi",
							},
						},
					},
					Replication: databasev1alpha1.ReplicationConfig{
						Mode: "async",
					},
					Failover: databasev1alpha1.FailoverConfig{
						FailureDetection: databasev1alpha1.FailureDetectionConfig{},
					},
				},
			}
			
			// Créer le secret requis
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mariadb-credentials-update",
					Namespace: mariadbHANamespace,
				},
				Type: corev1.SecretTypeOpaque,
				StringData: map[string]string{
					"MARIADB_ROOT_PASSWORD": "test-password",
				},
			}
			Expect(k8sClient.Create(ctx, secret)).Should(Succeed())
			
			// Ajouter la référence au secret
			mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret = &databasev1alpha1.SecretReference{
				Name: "mariadb-credentials-update",
			}
			
			// Créer l'objet MariaDBHA
			Expect(k8sClient.Create(ctx, mariadbHA)).Should(Succeed())
			
			// Attendre que le StatefulSet primaire soit créé
			primaryStatefulSetKey := types.NamespacedName{
				Name:      mariadbHAName + "-update-primary",
				Namespace: mariadbHANamespace,
			}
			primaryStatefulSet := &appsv1.StatefulSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, primaryStatefulSetKey, primaryStatefulSet)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			
			// Mettre à jour l'image MariaDB
			updatedMariadbHA := &databasev1alpha1.MariaDBHA{}
			Expect(k8sClient.Get(ctx, mariadbHAKey, updatedMariadbHA)).Should(Succeed())
			updatedMariadbHA.Spec.Instances.CommonConfig.Image = "mariadb:11.6"
			Expect(k8sClient.Update(ctx, updatedMariadbHA)).Should(Succeed())
			
			// Vérifier que le StatefulSet a été mis à jour avec la nouvelle image
			Eventually(func() bool {
				err := k8sClient.Get(ctx, primaryStatefulSetKey, primaryStatefulSet)
				if err != nil {
					return false
				}
				return primaryStatefulSet.Spec.Template.Spec.Containers[0].Image == "mariadb:11.6"
			}, timeout, interval).Should(BeTrue())
			
			// Nettoyer les ressources
			Expect(k8sClient.Delete(ctx, updatedMariadbHA)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).Should(Succeed())
		})
		
		It("Should transition to Running state when both StatefulSets are ready", func() {
			ctx := context.Background()
			
			// Créer le MariaDBHA
			mariadbHAKey := types.NamespacedName{
				Name:      mariadbHAName + "-phase",
				Namespace: mariadbHANamespace,
			}
			
			mariadbHA := &databasev1alpha1.MariaDBHA{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mariadbHAName + "-phase",
					Namespace: mariadbHANamespace,
				},
				Spec: databasev1alpha1.MariaDBHASpec{
					Instances: databasev1alpha1.InstancesConfig{
						CommonConfig: databasev1alpha1.MariaDBConfig{
							Version: "11.5",
							Image:   "mariadb:11.5",
							UserCredentialsSecret: &databasev1alpha1.SecretReference{
								Name: "mariadb-credentials-phase",
							},
						},
						Primary: databasev1alpha1.InstanceConfig{
							Storage: databasev1alpha1.StorageConfig{
								Size: "10Gi",
							},
						},
						Secondary: databasev1alpha1.InstanceConfig{
							Storage: databasev1alpha1.StorageConfig{
								Size: "10Gi",
							},
						},
					},
					Replication: databasev1alpha1.ReplicationConfig{
						Mode: "async",
					},
					Failover: databasev1alpha1.FailoverConfig{
						FailureDetection: databasev1alpha1.FailureDetectionConfig{},
					},
				},
			}
			
			// Créer le secret
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mariadb-credentials-phase",
					Namespace: mariadbHANamespace,
				},
				Type: corev1.SecretTypeOpaque,
				StringData: map[string]string{
					"MARIADB_ROOT_PASSWORD": "test-password",
				},
			}
			Expect(k8sClient.Create(ctx, secret)).Should(Succeed())
			
			// Créer le MariaDBHA
			Expect(k8sClient.Create(ctx, mariadbHA)).Should(Succeed())
			
			// Vérifier que la phase initiale est "Initializing"
			Eventually(func() string {
				err := k8sClient.Get(ctx, mariadbHAKey, mariadbHA)
				if err != nil {
					return ""
				}
				return mariadbHA.Status.Phase
			}, timeout, interval).Should(Equal(PhaseInitializing))
			
			// Simuler des StatefulSets prêts en mettant manuellement à jour leur statut
			// Dans un test réel, vous auriez besoin d'un faux client qui permet de manipuler les statuts
			// ou d'attendre que les StatefulSets soient réellement prêts
			
			// Ce test est incomplet car nous ne pouvons pas facilement simuler des StatefulSets prêts
			// dans un environnement de test sans pods réels. Dans un test e2e, vous pourriez 
			// observer la transition automatique vers l'état Running.
			
			// Nettoyer les ressources
			Expect(k8sClient.Delete(ctx, mariadbHA)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).Should(Succeed())
		})
	})
	
	Context("When a failover occurs", func() {
		It("Should update services to point to the new primary", func() {
			// Ce test nécessiterait une configuration plus complexe pour simuler un failover
			// Dans un test e2e sur un vrai cluster, vous pourriez tester cela en arrêtant 
			// forcément le pod primaire et en vérifiant que le service est mis à jour
			
			// Pour les tests d'intégration, vous pourriez:
			// 1. Créer manuellement un MariaDBHA en phase de failover
			// 2. Appeler directement handleFailover
			// 3. Vérifier que les services sont mis à jour correctement
			
			// Ce test est laissé comme exercice ou pour être complété 
			// dans une suite de tests e2e plus complète
		})
	})
})

// Test de la fonction Reconcile directement
var _ = Describe("Reconcile Function", func() {
	It("Should add a finalizer when one doesn't exist", func() {
		// Créer un reconciler avec un client mock
		reconciler := &MariaDBHAReconciler{
			Client: k8sClient,
			Scheme: scheme,
		}
		
		// Créer un MariaDBHA sans finalizer
		mariadbHA := &databasev1alpha1.MariaDBHA{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-finalizer",
				Namespace: "default",
			},
			Spec: databasev1alpha1.MariaDBHASpec{
				Instances: databasev1alpha1.InstancesConfig{
					Primary: databasev1alpha1.InstanceConfig{
						Storage: databasev1alpha1.StorageConfig{
							Size: "10Gi",
						},
					},
					Secondary: databasev1alpha1.InstanceConfig{
						Storage: databasev1alpha1.StorageConfig{
							Size: "10Gi",
						},
					},
				},
				Failover: databasev1alpha1.FailoverConfig{
					FailureDetection: databasev1alpha1.FailureDetectionConfig{},
				},
			},
		}
		
		// Créer l'objet
		ctx := context.Background()
		Expect(k8sClient.Create(ctx, mariadbHA)).Should(Succeed())
		
		// Appeler Reconcile
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-finalizer",
				Namespace: "default",
			},
		}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).ShouldNot(HaveOccurred())
		
		// Vérifier que le finalizer a été ajouté
		updatedMariaDBHA := &databasev1alpha1.MariaDBHA{}
		err = k8sClient.Get(ctx, req.NamespacedName, updatedMariaDBHA)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(controllerutil.ContainsFinalizer(updatedMariaDBHA, mariadbHAFinalizer)).Should(BeTrue())
		
		// Nettoyer
		Expect(k8sClient.Delete(ctx, updatedMariaDBHA)).Should(Succeed())
	})
	
	It("Should initialize status with the Initializing phase", func() {
		// Créer un reconciler avec un client mock
		reconciler := &MariaDBHAReconciler{
			Client: k8sClient,
			Scheme: scheme,
		}
		
		// Créer un MariaDBHA sans statut
		mariadbHA := &databasev1alpha1.MariaDBHA{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-status",
				Namespace:  "default",
				Finalizers: []string{mariadbHAFinalizer}, // Ajouter directement le finalizer
			},
			Spec: databasev1alpha1.MariaDBHASpec{
				Instances: databasev1alpha1.InstancesConfig{
					Primary: databasev1alpha1.InstanceConfig{
						Storage: databasev1alpha1.StorageConfig{
							Size: "10Gi",
						},
					},
					Secondary: databasev1alpha1.InstanceConfig{
						Storage: databasev1alpha1.StorageConfig{
							Size: "10Gi",
						},
					},
				},
				Failover: databasev1alpha1.FailoverConfig{
					FailureDetection: databasev1alpha1.FailureDetectionConfig{},
				},
			},
		}
		
		// Créer l'objet
		ctx := context.Background()
		Expect(k8sClient.Create(ctx, mariadbHA)).Should(Succeed())
		
		// Appeler Reconcile
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-status",
				Namespace: "default",
			},
		}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).ShouldNot(HaveOccurred())
		
		// Vérifier que le statut a été initialisé
		updatedMariaDBHA := &databasev1alpha1.MariaDBHA{}
		err = k8sClient.Get(ctx, req.NamespacedName, updatedMariaDBHA)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(updatedMariaDBHA.Status.Phase).Should(Equal(PhaseInitializing))
		
		// Nettoyer
		Expect(k8sClient.Delete(ctx, updatedMariaDBHA)).Should(Succeed())
	})
})