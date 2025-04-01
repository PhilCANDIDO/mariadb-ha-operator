// internal/controller/health_check_test.go
package controller

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	databasev1alpha1 "github.com/PhilCANDIDO/mariadb-ha-operator/api/v1alpha1"
)

func setupTestLogger() logr.Logger {
	return zap.New(zap.UseDevMode(true))
}

func setupTestScheme() *runtime.Scheme {
	s := scheme.Scheme
	_ = databasev1alpha1.AddToScheme(s)
	return s
}

func createTestMariaDBHA() *databasev1alpha1.MariaDBHA {
	return &databasev1alpha1.MariaDBHA{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-mariadb",
			Namespace: "default",
		},
		Spec: databasev1alpha1.MariaDBHASpec{
			Instances: databasev1alpha1.InstancesConfig{
				CommonConfig: databasev1alpha1.MariaDBConfig{
					UserCredentialsSecret: &databasev1alpha1.SecretReference{
						Name: "test-credentials",
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
				MaxLagSeconds: int32Ptr(30),
			},
			Failover: databasev1alpha1.FailoverConfig{
				FailureDetection: databasev1alpha1.FailureDetectionConfig{
					TimeoutSeconds: int32Ptr(2),
				},
			},
		},
	}
}

func createTestPods() []runtime.Object {
	return []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-mariadb-primary-0",
				Namespace: "default",
				Labels: map[string]string{
					"app":                     "mariadb",
					"mariadb.cpf-it.fr/role":  "primary",
					"mariadb.cpf-it.fr/cluster": "test-mariadb",
				},
			},
			Status: corev1.PodStatus{
				PodIP: "192.168.1.10",
				Phase: corev1.PodRunning,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-mariadb-secondary-0",
				Namespace: "default",
				Labels: map[string]string{
					"app":                     "mariadb",
					"mariadb.cpf-it.fr/role":  "secondary",
					"mariadb.cpf-it.fr/cluster": "test-mariadb",
				},
			},
			Status: corev1.PodStatus{
				PodIP: "192.168.1.11",
				Phase: corev1.PodRunning,
			},
		},
	}
}

func createTestSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"MARIADB_USER":     []byte("testuser"),
			"MARIADB_PASSWORD": []byte("testpassword"),
		},
	}
}

// Test pour la fonction getMariaDBConnectionInfo
func TestGetMariaDBConnectionInfo(t *testing.T) {
	// Configurer le contexte et le logger pour les tests
	ctx := context.TODO()
	_ = setupTestLogger()
	
	// Créer un scheme de test
	s := setupTestScheme()
	
	// Configurer un client fake avec des objets préremplis
	client := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(createTestMariaDBHA(), createTestSecret()).
		WithRuntimeObjects(createTestPods()...).
		Build()
	
	// Cas de test : récupérer les infos de connexion au primaire
	t.Run("Primary Connection Info", func(t *testing.T) {
		mariadbHA := createTestMariaDBHA()
		info, err := getMariaDBConnectionInfo(ctx, client, mariadbHA, true)
		
		assert.NoError(t, err, "Should not return an error")
		assert.NotNil(t, info, "Should return connection info")
		assert.Equal(t, "192.168.1.10", info.Host, "Should return the correct host IP")
		assert.Equal(t, 3306, info.Port, "Should return the default port")
		assert.Equal(t, "testuser", info.Username, "Should return the correct username")
		assert.Equal(t, "testpassword", info.Password, "Should return the correct password")
		assert.Equal(t, "mysql", info.Database, "Should return the default database")
	})
	
	// Cas de test : récupérer les infos de connexion au secondaire
	t.Run("Secondary Connection Info", func(t *testing.T) {
		mariadbHA := createTestMariaDBHA()
		info, err := getMariaDBConnectionInfo(ctx, client, mariadbHA, false)
		
		assert.NoError(t, err, "Should not return an error")
		assert.NotNil(t, info, "Should return connection info")
		assert.Equal(t, "192.168.1.11", info.Host, "Should return the correct host IP")
	})
	
	// Cas de test : pod qui n'existe pas
	t.Run("Non-existent Pod", func(t *testing.T) {
		mariadbHA := createTestMariaDBHA()
		mariadbHA.Name = "nonexistent"
		_, err := getMariaDBConnectionInfo(ctx, client, mariadbHA, true)
		
		assert.Error(t, err, "Should return an error for non-existent pod")
	})
}

// Test pour checkReplicationStatus avec une mock de la base de données
func TestCheckReplicationStatus(t *testing.T) {
	// Créer une mock de la base de données SQL
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Error creating sql mock: %v", err)
	}
	defer db.Close()
	
	// Remplacer la fonction connectToDatabase pour nos tests
	originalConnectToDatabase := connectToDatabase
	defer func() { connectToDatabase = originalConnectToDatabase }()
	
	connectToDatabase = func(info *MariaDBConnectionInfo) (*sql.DB, error) {
		return db, nil
	}
	
	// Cas de test : réplication saine
	t.Run("Healthy Replication", func(t *testing.T) {
		// Configurer les colonnes attendues pour le résultat de la requête
		columns := []string{"Slave_IO_Running", "Slave_SQL_Running", "Last_IO_Errno", "Last_SQL_Errno", "Seconds_Behind_Master"}
		
		// Créer des lignes mock avec des valeurs saines
		rows := sqlmock.NewRows(columns).
			AddRow("Yes", "Yes", "0", "0", "5")
		
		// Configurer la première tentative avec SHOW REPLICA STATUS
		mock.ExpectQuery("SHOW REPLICA STATUS").WillReturnRows(rows)
		
		info := &MariaDBConnectionInfo{
			Host:     "192.168.1.11",
			Port:     3306,
			Username: "testuser",
			Password: "testpassword",
			Database: "mysql",
		}
		
		status, err := checkReplicationStatus(info, 5*time.Second)
		
		assert.NoError(t, err, "Should not return an error")
		assert.True(t, status.SlaveRunning, "Replication should be running")
		assert.Equal(t, int32(5), *status.SecondsBehindMaster, "Should report correct lag")
	})
	
	// Cas de test : problème de réplication
	t.Run("Replication Problem", func(t *testing.T) {
		// Réinitialiser les attentes
		mock.ExpectationsWereMet()
		
		// Configurer les colonnes attendues pour le résultat de la requête
		columns := []string{"Slave_IO_Running", "Slave_SQL_Running", "Last_IO_Errno", "Last_SQL_Errno", "Seconds_Behind_Master"}
		
		// Créer des lignes mock avec des valeurs problématiques
		rows := sqlmock.NewRows(columns).
			AddRow("No", "Yes", "1236", "0", "NULL")
		
		// Configurer la tentative avec SHOW REPLICA STATUS
		mock.ExpectQuery("SHOW REPLICA STATUS").WillReturnRows(rows)
		
		info := &MariaDBConnectionInfo{
			Host:     "192.168.1.11",
			Port:     3306,
			Username: "testuser",
			Password: "testpassword",
			Database: "mysql",
		}
		
		status, err := checkReplicationStatus(info, 5*time.Second)
		
		assert.NoError(t, err, "Should not return an error")
		assert.False(t, status.SlaveRunning, "Replication should not be running")
		assert.Equal(t, 1236, status.LastIOErrno, "Should report correct IO error number")
		assert.Nil(t, status.SecondsBehindMaster, "Seconds behind master should be nil for NULL value")
	})
	
	// Cas de test : syntaxe alternative (SHOW SLAVE STATUS)
	t.Run("Alternative Syntax", func(t *testing.T) {
		// Réinitialiser les attentes
		mock.ExpectationsWereMet()
		
		// SHOW REPLICA STATUS échoue
		mock.ExpectQuery("SHOW REPLICA STATUS").WillReturnError(fmt.Errorf("unknown command"))
		
		// Configurer les colonnes pour SHOW SLAVE STATUS
		columns := []string{"Slave_IO_Running", "Slave_SQL_Running", "Last_IO_Errno", "Last_SQL_Errno", "Seconds_Behind_Master"}
		
		// Créer des lignes mock avec des valeurs
		rows := sqlmock.NewRows(columns).
			AddRow("Yes", "Yes", "0", "0", "10")
		
		// SHOW SLAVE STATUS réussit
		mock.ExpectQuery("SHOW SLAVE STATUS").WillReturnRows(rows)
		
		info := &MariaDBConnectionInfo{
			Host:     "192.168.1.11",
			Port:     3306,
			Username: "testuser",
			Password: "testpassword",
			Database: "mysql",
		}
		
		status, err := checkReplicationStatus(info, 5*time.Second)
		
		assert.NoError(t, err, "Should not return an error")
		assert.True(t, status.SlaveRunning, "Replication should be running")
		assert.Equal(t, int32(10), *status.SecondsBehindMaster, "Should report correct lag")
	})
}

// Test pour la fonction resetFailureCount
func TestFailureCountFunctions(t *testing.T) {
	// Créer un MariaDBHA avec des compteurs d'échec
	mariadbHA := createTestMariaDBHA()
	mariadbHA.Annotations = map[string]string{
		PrimaryFailCountAnnotation:   "3",
		SecondaryFailCountAnnotation: "2",
	}
	
	// Tester getFailureCount
	t.Run("Get Failure Count", func(t *testing.T) {
		primaryCount := getFailureCount(mariadbHA, true)
		secondaryCount := getFailureCount(mariadbHA, false)
		
		assert.Equal(t, 3, primaryCount, "Should return correct primary failure count")
		assert.Equal(t, 2, secondaryCount, "Should return correct secondary failure count")
	})
	
	// Tester saveFailureCount
	t.Run("Save Failure Count", func(t *testing.T) {
		saveFailureCount(mariadbHA, true, 5)
		saveFailureCount(mariadbHA, false, 4)
		
		assert.Equal(t, "5", mariadbHA.Annotations[PrimaryFailCountAnnotation], "Should save primary failure count")
		assert.Equal(t, "4", mariadbHA.Annotations[SecondaryFailCountAnnotation], "Should save secondary failure count")
	})
	
	// Tester resetFailureCount
	t.Run("Reset Failure Count", func(t *testing.T) {
		resetFailureCount(mariadbHA, true)
		
		_, primaryExists := mariadbHA.Annotations[PrimaryFailCountAnnotation]
		assert.False(t, primaryExists, "Primary failure count should be removed")
		assert.Equal(t, "4", mariadbHA.Annotations[SecondaryFailCountAnnotation], "Secondary failure count should remain")
		
		resetFailureCount(mariadbHA, false)
		_, secondaryExists := mariadbHA.Annotations[SecondaryFailCountAnnotation]
		assert.False(t, secondaryExists, "Secondary failure count should be removed")
	})
}