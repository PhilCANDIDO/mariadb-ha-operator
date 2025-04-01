// internal/controller/failover_test.go
package controller

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	databasev1alpha1 "github.com/PhilCANDIDO/mariadb-ha-operator/api/v1alpha1"
)

// Mock de la fonction connectToDatabase pour les tests
func setupMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Error creating sql mock: %v", err)
	}

	originalConnectToDatabase := connectToDatabase
	connectToDatabase = func(info *MariaDBConnectionInfo) (*sql.DB, error) {
		return db, nil
	}

	cleanup := func() {
		db.Close()
		connectToDatabase = originalConnectToDatabase
	}

	return db, mock, cleanup
}

// Test pour stopReplication
func TestStopReplication(t *testing.T) {
	db, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.TODO()
	info := &MariaDBConnectionInfo{
		Host:     "192.168.1.11",
		Port:     3306,
		Username: "testuser",
		Password: "testpassword",
		Database: "mysql",
	}

	// Cas de test : arrêt réussi avec syntaxe moderne
	t.Run("Successful Stop with Modern Syntax", func(t *testing.T) {
		// Configurer les attentes
		mock.ExpectExec("STOP REPLICA").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("RESET SLAVE ALL").WillReturnError(sql.ErrNoRows)
		mock.ExpectExec("RESET REPLICA ALL").WillReturnResult(sqlmock.NewResult(0, 0))

		err := stopReplication(ctx, info)
		assert.NoError(t, err, "Should not return an error")
	})

	// Cas de test : arrêt réussi avec ancienne syntaxe
	t.Run("Successful Stop with Legacy Syntax", func(t *testing.T) {
		// Réinitialiser les attentes
		mock.ExpectationsWereMet()

		// Configurer les attentes pour l'ancienne syntaxe
		mock.ExpectExec("STOP REPLICA").WillReturnError(sql.ErrNoRows)
		mock.ExpectExec("STOP SLAVE").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("RESET SLAVE ALL").WillReturnResult(sqlmock.NewResult(0, 0))

		err := stopReplication(ctx, info)
		assert.NoError(t, err, "Should not return an error")
	})

	// Cas de test : échec complet
	t.Run("Complete Failure", func(t *testing.T) {
		// Réinitialiser les attentes
		mock.ExpectationsWereMet()

		// Simuler un échec des deux commandes
		mock.ExpectExec("STOP REPLICA").WillReturnError(sql.ErrConnDone)
		mock.ExpectExec("STOP SLAVE").WillReturnError(sql.ErrConnDone)

		err := stopReplication(ctx, info)
		assert.Error(t, err, "Should return an error when both commands fail")
	})
}

// Test pour promoteSecondaryToPrimary
func TestPromoteSecondaryToPrimary(t *testing.T) {
	db, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.TODO()
	info := &MariaDBConnectionInfo{
		Host:     "192.168.1.11",
		Port:     3306,
		Username: "testuser",
		Password: "testpassword",
		Database: "mysql",
	}

	// Cas de test : promotion en mode lecture/écriture
	t.Run("Promote to Read-Write", func(t *testing.T) {
		mariadbHA := createTestMariaDBHA()
		mariadbHA.Spec.Failover.ReadOnlyMode = boolPtr(false)

		// Configurer les attentes
		mock.ExpectExec("SET GLOBAL read_only = OFF").WillReturnResult(sqlmock.NewResult(0, 0))

		err := promoteSecondaryToPrimary(ctx, info, mariadbHA)
		assert.NoError(t, err, "Should not return an error")
	})

	// Cas de test : promotion en mode lecture seule
	t.Run("Promote to Read-Only", func(t *testing.T) {
		// Réinitialiser les attentes
		mock.ExpectationsWereMet()

		mariadbHA := createTestMariaDBHA()
		mariadbHA.Spec.Failover.ReadOnlyMode = boolPtr(true)

		// Configurer les attentes
		mock.ExpectExec("SET GLOBAL read_only = ON").WillReturnResult(sqlmock.NewResult(0, 0))

		err := promoteSecondaryToPrimary(ctx, info, mariadbHA)
		assert.NoError(t, err, "Should not return an error")
	})

	// Cas de test : échec de la commande
	t.Run("Command Failure", func(t *testing.T) {
		// Réinitialiser les attentes
		mock.ExpectationsWereMet()

		mariadbHA := createTestMariaDBHA()
		mariadbHA.Spec.Failover.ReadOnlyMode = boolPtr(false)

		// Simuler un échec de la commande
		mock.ExpectExec("SET GLOBAL read_only = OFF").WillReturnError(sql.ErrConnDone)

		err := promoteSecondaryToPrimary(ctx, info, mariadbHA)
		assert.Error(t, err, "Should return an error when command fails")
	})
}

// Test pour updateServicesAfterFailover
func TestUpdateServicesAfterFailover(t *testing.T) {
	// Configurer le contexte et le scheme
	ctx := context.TODO()
	s := setupTestScheme()

	// Créer un MariaDBHA pour le test
	mariadbHA := createTestMariaDBHA()

	// Créer un service qui pointe vers le primaire
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-mariadb-mariadb",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":                     "mariadb",
				"mariadb.cpf-it.fr/role":  "primary",
				"mariadb.cpf-it.fr/cluster": "test-mariadb",
			},
		},
	}

	// Créer le pod secondaire (qui deviendra le nouveau primaire)
	secondaryPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-mariadb-secondary-0",
			Namespace: "default",
			Labels: map[string]string{
				"app":                     "mariadb",
				"mariadb.cpf-it.fr/role":  "secondary",
				"mariadb.cpf-it.fr/cluster": "test-mariadb",
				"statefulset.kubernetes.io/pod-name": "test-mariadb-secondary-0",
			},
		},
	}

	// Créer un client fake avec les objets préparés
	client := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(mariadbHA, svc, secondaryPod).
		Build()

	// Créer le réconciliateur
	recorder := &record.FakeRecorder{}
	r := &MariaDBHAReconciler{
		Client:   client,
		Scheme:   s,
		Recorder: recorder,
	}

	// Test : Mise à jour du service pour pointer vers le nouveau primaire
	t.Run("Update Service Selectors", func(t *testing.T) {
		err := r.updateServicesAfterFailover(ctx, mariadbHA)
		assert.NoError(t, err, "Should not return an error")

		// Vérifier que le service a bien été mis à jour
		updatedSvc := &corev1.Service{}
		err = client.Get(ctx, types.NamespacedName{Name: "test-mariadb-mariadb", Namespace: "default"}, updatedSvc)
		assert.NoError(t, err, "Should be able to get the service")

		// Vérifier que le sélecteur a été mis à jour pour pointer vers le pod secondaire
		assert.Equal(t, secondaryPod.Labels, updatedSvc.Spec.Selector, 
			"Service selector should point to the secondary pod")
	})
}

// Test pour isFailoverAllowed
func TestIsFailoverAllowed(t *testing.T) {
	ctx := context.TODO()
	s := setupTestScheme()
	client := fake.NewClientBuilder().WithScheme(s).Build()
	recorder := &record.FakeRecorder{}
	r := &MariaDBHAReconciler{
		Client:   client,
		Scheme:   s,
		Recorder: recorder,
	}

	// Cas de test : failover automatique désactivé
	t.Run("Automatic Failover Disabled", func(t *testing.T) {
		mariadbHA := createTestMariaDBHA()
		mariadbHA.Spec.Replication.AutomaticFailover = boolPtr(false)

		allowed := r.isFailoverAllowed(ctx, mariadbHA)
		assert.False(t, allowed, "Failover should not be allowed when automatic failover is disabled")
	})

	// Cas de test : failover en cours
	t.Run("Failover In Progress", func(t *testing.T) {
		mariadbHA := createTestMariaDBHA()
		mariadbHA.Spec.Replication.AutomaticFailover = boolPtr(true)
		mariadbHA.Status.Phase = PhaseFailover

		allowed := r.isFailoverAllowed(ctx, mariadbHA)
		assert.False(t, allowed, "Failover should not be allowed when a failover is already in progress")
	})

	// Cas de test : délai minimum non écoulé
	t.Run("Minimum Interval Not Elapsed", func(t *testing.T) {
		mariadbHA := createTestMariaDBHA()
		mariadbHA.Spec.Replication.AutomaticFailover = boolPtr(true)
		mariadbHA.Status.Phase = PhaseRunning
		// Dernier failover il y a 2 minutes
		lastFailover := metav1.NewTime(time.Now().Add(-2 * time.Minute))
		mariadbHA.Status.LastFailoverTime = &lastFailover
		// Délai minimum de 5 minutes
		mariadbHA.Spec.Failover.MinimumIntervalSeconds = int32Ptr(300)

		allowed := r.isFailoverAllowed(ctx, mariadbHA)
		assert.False(t, allowed, "Failover should not be allowed when minimum interval has not elapsed")
	})

	// Cas de test : toutes les conditions sont réunies
	t.Run("All Conditions Met", func(t *testing.T) {
		mariadbHA := createTestMariaDBHA()
		mariadbHA.Spec.Replication.AutomaticFailover = boolPtr(true)
		mariadbHA.Status.Phase = PhaseRunning
		// Dernier failover il y a 10 minutes
		lastFailover := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		mariadbHA.Status.LastFailoverTime = &lastFailover
		// Délai minimum de 5 minutes
		mariadbHA.Spec.Failover.MinimumIntervalSeconds = int32Ptr(300)

		allowed := r.isFailoverAllowed(ctx, mariadbHA)
		assert.True(t, allowed, "Failover should be allowed when all conditions are met")
	})
}