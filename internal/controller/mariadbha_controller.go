// internal/controller/mariadbha_controller.go
package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	databasev1alpha1 "github.com/PhilCANDIDO/mariadb-ha-operator/api/v1alpha1"
)

// MariaDBHAReconciler réconcilie une ressource MariaDBHA
type MariaDBHAReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Autorisations RBAC
//+kubebuilder:rbac:groups=database.cpf-it.fr,resources=mariadbhas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=database.cpf-it.fr,resources=mariadbhas/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=database.cpf-it.fr,resources=mariadbhas/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Constantes
const (
	// Finalizer pour nettoyer les ressources lors de la suppression
	mariadbHAFinalizer = "database.cpf-it.fr/finalizer"

	// Conditions
	conditionPrimaryReady      = "PrimaryReady"
	conditionSecondaryReady    = "SecondaryReady"
	conditionReplicationActive = "ReplicationActive"

	// Intervalles
	defaultSyncPeriod     = 10 * time.Second
	defaultHealthCheckTTL = 5 * time.Second
)

// Phases du cluster
const (
	PhaseInitializing   = "Initializing"
	PhaseRunning        = "Running"
	PhaseFailing        = "Failing"
	PhaseFailover       = "Failover"
	PhaseRecovering     = "Recovering"
	PhaseDegraded       = "Degraded"
	PhaseMaintenanceMode = "MaintenanceMode"
)

// Reconcile est la fonction principale du contrôleur
func (r *MariaDBHAReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Réconciliation MariaDBHA démarrée", "namespace", req.Namespace, "name", req.Name)

	// Récupérer l'instance MariaDBHA
	mariadbHA := &databasev1alpha1.MariaDBHA{}
	err := r.Get(ctx, req.NamespacedName, mariadbHA)
	if err != nil {
		if errors.IsNotFound(err) {
			// La ressource a été supprimée, plus rien à faire
			logger.Info("MariaDBHA supprimé")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Impossible de récupérer MariaDBHA")
		return ctrl.Result{}, err
	}

	// Ajouter le finalizer s'il n'existe pas déjà
	if !controllerutil.ContainsFinalizer(mariadbHA, mariadbHAFinalizer) {
		controllerutil.AddFinalizer(mariadbHA, mariadbHAFinalizer)
		if err := r.Update(ctx, mariadbHA); err != nil {
			logger.Error(err, "Impossible d'ajouter le finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Finalizer ajouté")
		return ctrl.Result{}, nil
	}

	// Gestion de la suppression
	if !mariadbHA.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, mariadbHA)
	}

	// Initialiser le statut si nécessaire
	if mariadbHA.Status.Phase == "" {
		logger.Info("Initialisation du status")
		mariadbHA.Status.Phase = PhaseInitializing
		err = r.Status().Update(ctx, mariadbHA)
		if err != nil {
			logger.Error(err, "Impossible de mettre à jour le statut initial")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Logique principale de réconciliation
	result, err := r.reconcileStatefulSets(ctx, mariadbHA)
	if err != nil || result.Requeue {
		return result, err
	}

	result, err = r.reconcileServices(ctx, mariadbHA)
	if err != nil || result.Requeue {
		return result, err
	}

	result, err = r.reconcileReplication(ctx, mariadbHA)
	if err != nil || result.Requeue {
		return result, err
	}

	// Vérifier l'état de santé si en phase Running ou Degraded
	if mariadbHA.Status.Phase == PhaseRunning || mariadbHA.Status.Phase == PhaseDegraded {
		return r.checkHealth(ctx, mariadbHA)
	}

	// Vérifier si nous sommes en phase de failover
	if mariadbHA.Status.Phase == PhaseFailover {
		return r.handleFailover(ctx, mariadbHA)
	}

	// Vérifier si nous sommes en phase de récupération
	if mariadbHA.Status.Phase == PhaseRecovering {
		return r.handleRecovery(ctx, mariadbHA)
	}

	// Par défaut, re-vérifier régulièrement
	return ctrl.Result{RequeueAfter: defaultSyncPeriod}, nil
}

// handleDeletion gère la suppression de la ressource MariaDBHA
func (r *MariaDBHAReconciler) handleDeletion(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Gestion de la suppression", "name", mariadbHA.Name)

	// Supprimer les ressources créées (StatefulSets, Services, etc.)
	// TODO: Ajouter la logique de nettoyage

	// Une fois tout nettoyé, retirer le finalizer
	controllerutil.RemoveFinalizer(mariadbHA, mariadbHAFinalizer)
	if err := r.Update(ctx, mariadbHA); err != nil {
		logger.Error(err, "Impossible de retirer le finalizer")
		return ctrl.Result{}, err
	}

	logger.Info("Finalizer retiré, ressource prête à être supprimée")
	return ctrl.Result{}, nil
}

// reconcileStatefulSets crée ou met à jour les StatefulSets pour les instances primaire et secondaire
func (r *MariaDBHAReconciler) reconcileStatefulSets(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Réconciliation des StatefulSets")

	// TODO: Implémenter la logique de gestion des StatefulSets
	// 1. Créer/mettre à jour le StatefulSet primaire
	// 2. Créer/mettre à jour le StatefulSet secondaire
	// 3. Vérifier l'état des StatefulSets

	return ctrl.Result{}, nil
}

// reconcileServices crée ou met à jour les Services pour les instances
func (r *MariaDBHAReconciler) reconcileServices(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Réconciliation des Services")

	// TODO: Implémenter la logique de gestion des Services
	// 1. Créer/mettre à jour le Service pour le primaire
	// 2. Créer/mettre à jour le Service pour le secondaire
	// 3. Créer/mettre à jour le Service d'accès général (qui pointe vers le primaire)

	return ctrl.Result{}, nil
}

// reconcileReplication configure la réplication entre primaire et secondaire
func (r *MariaDBHAReconciler) reconcileReplication(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Réconciliation de la réplication")

	// TODO: Implémenter la logique de configuration de la réplication
	// 1. Vérifier si les deux instances sont prêtes
	// 2. Configurer la réplication si nécessaire
	// 3. Vérifier l'état de la réplication

	return ctrl.Result{}, nil
}

// checkHealth surveille l'état de santé des instances MariaDB
func (r *MariaDBHAReconciler) checkHealth(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Vérification de l'état de santé du cluster")

	// Vérifier si un health check récent existe déjà
	// Si non, lancer un health check
	healthCheckResult, err := r.performHealthCheck(ctx, mariadbHA)
	if err != nil {
		logger.Error(err, "Erreur lors du health check")
		return ctrl.Result{}, err
	}

	// Analyser les résultats du health check
	if !healthCheckResult.primaryHealthy {
		// Incrémenter le compteur d'échecs pour le primaire
		// Si le seuil est dépassé, déclencher un failover
		return r.handlePrimaryFailure(ctx, mariadbHA, healthCheckResult)
	}

	// Vérifier l'état du secondaire
	if !healthCheckResult.secondaryHealthy {
		// Marquer le cluster comme dégradé
		if mariadbHA.Status.Phase != PhaseDegraded {
			mariadbHA.Status.Phase = PhaseDegraded
			r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "SecondaryFailure", 
				"Le serveur secondaire n'est pas disponible, le cluster est dégradé")
			if err := r.Status().Update(ctx, mariadbHA); err != nil {
				logger.Error(err, "Impossible de mettre à jour le statut")
				return ctrl.Result{}, err
			}
		}
	} else if !healthCheckResult.replicationHealthy {
		// Problème de réplication, mais les deux serveurs sont en vie
		if mariadbHA.Status.Phase != PhaseDegraded {
			mariadbHA.Status.Phase = PhaseDegraded
			r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "ReplicationFailure", 
				"La réplication n'est pas fonctionnelle, le cluster est dégradé")
			if err := r.Status().Update(ctx, mariadbHA); err != nil {
				logger.Error(err, "Impossible de mettre à jour le statut")
				return ctrl.Result{}, err
			}
		}
	} else if mariadbHA.Status.Phase != PhaseRunning {
		// Tout est fonctionnel, marquer comme Running si ce n'est pas déjà le cas
		mariadbHA.Status.Phase = PhaseRunning
		r.Recorder.Event(mariadbHA, corev1.EventTypeNormal, "ClusterHealthy", 
			"Le cluster MariaDB HA fonctionne normalement")
		if err := r.Status().Update(ctx, mariadbHA); err != nil {
			logger.Error(err, "Impossible de mettre à jour le statut")
			return ctrl.Result{}, err
		}
	}

	// Mettre à jour les métriques de réplication
	if healthCheckResult.replicationLag != nil {
		mariadbHA.Status.ReplicationLag = healthCheckResult.replicationLag
		if err := r.Status().Update(ctx, mariadbHA); err != nil {
			logger.Error(err, "Impossible de mettre à jour le statut de réplication")
			return ctrl.Result{}, err
		}
	}

	// Programmer la prochaine vérification
	return ctrl.Result{RequeueAfter: defaultHealthCheckTTL}, nil
}

// HealthCheckResult contient les résultats d'un health check
type HealthCheckResult struct {
	primaryHealthy    bool
	secondaryHealthy  bool
	replicationHealthy bool
	replicationLag    *int32
	primaryFailCount  int
	secondaryFailCount int
}

// performHealthCheck exécute les vérifications de santé sur les instances MariaDB
func (r *MariaDBHAReconciler) performHealthCheck(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (HealthCheckResult, error) {
	logger := log.FromContext(ctx)
	logger.Info("Exécution des health checks")

	result := HealthCheckResult{
		primaryHealthy:    false,
		secondaryHealthy:  false,
		replicationHealthy: false,
	}

	// TODO: Implémenter la logique des health checks
	// 1. Vérifier la connexion TCP
	// 2. Exécuter une requête SQL simple
	// 3. Vérifier l'état de la réplication
	// 4. Mesurer le lag de réplication

	// Exemple de logique pour les tests:
	result.primaryHealthy = true
	result.secondaryHealthy = true
	result.replicationHealthy = true
	lagValue := int32(10) // Exemple: 10 secondes de retard
	result.replicationLag = &lagValue

	return result, nil
}

// handlePrimaryFailure gère la défaillance du serveur primaire
func (r *MariaDBHAReconciler) handlePrimaryFailure(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA, healthCheck HealthCheckResult) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Gestion de la défaillance du primaire")

	// Si le basculement automatique n'est pas activé, juste marquer comme défaillant
	autoFailover := mariadbHA.Spec.Replication.AutomaticFailover != nil && *mariadbHA.Spec.Replication.AutomaticFailover
	if !autoFailover {
		if mariadbHA.Status.Phase != PhaseFailing {
			mariadbHA.Status.Phase = PhaseFailing
			r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "PrimaryFailure", 
				"Le serveur primaire n'est pas disponible, intervention manuelle requise")
			if err := r.Status().Update(ctx, mariadbHA); err != nil {
				logger.Error(err, "Impossible de mettre à jour le statut")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: defaultHealthCheckTTL}, nil
	}

	// Vérifier si le failover peut être déclenché
	failureThreshold := int32(3) // Valeur par défaut
	if mariadbHA.Spec.Failover.FailureDetection.FailureThresholdCount != nil {
		failureThreshold = *mariadbHA.Spec.Failover.FailureDetection.FailureThresholdCount
	}

	// Incrémenter le compteur d'échecs et vérifier le seuil
	healthCheck.primaryFailCount++
	if int32(healthCheck.primaryFailCount) < failureThreshold {
		logger.Info("Seuil de défaillance non atteint", 
			"count", healthCheck.primaryFailCount,
			"threshold", failureThreshold)
		return ctrl.Result{RequeueAfter: defaultHealthCheckTTL}, nil
	}

	// Vérifier si le secondaire est sain avant de déclencher le failover
	if !healthCheck.secondaryHealthy {
		logger.Info("Failover impossible: le secondaire n'est pas sain")
		mariadbHA.Status.Phase = PhaseFailing
		r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverBlocked", 
			"Failover impossible car le secondaire n'est pas disponible")
		if err := r.Status().Update(ctx, mariadbHA); err != nil {
			logger.Error(err, "Impossible de mettre à jour le statut")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultHealthCheckTTL}, nil
	}

	// Vérifier le délai minimum entre les failovers
	if mariadbHA.Status.LastFailoverTime != nil {
		minIntervalSeconds := int32(300) // 5 minutes par défaut
		if mariadbHA.Spec.Failover.MinimumIntervalSeconds != nil {
			minIntervalSeconds = *mariadbHA.Spec.Failover.MinimumIntervalSeconds
		}
		
		lastFailover := mariadbHA.Status.LastFailoverTime.Time
		minTimeBetweenFailovers := time.Duration(minIntervalSeconds) * time.Second
		if time.Since(lastFailover) < minTimeBetweenFailovers {
			logger.Info("Délai minimum entre failovers non écoulé",
				"lastFailover", lastFailover,
				"minimumInterval", minTimeBetweenFailovers)
			return ctrl.Result{RequeueAfter: defaultHealthCheckTTL}, nil
		}
	}

	// Déclencher le failover
	logger.Info("Déclenchement du failover")
	mariadbHA.Status.Phase = PhaseFailover
	r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverStarted", 
		"Démarrage du failover du primaire vers le secondaire")
	
	// Mettre à jour le statut
	if err := r.Status().Update(ctx, mariadbHA); err != nil {
		logger.Error(err, "Impossible de mettre à jour le statut pour le failover")
		return ctrl.Result{}, err
	}

	// Réenchaîner immédiatement pour traiter la logique de failover
	return ctrl.Result{Requeue: true}, nil
}

// handleFailover gère le processus de failover
func (r *MariaDBHAReconciler) handleFailover(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Exécution du failover")

	// TODO: Implémenter la logique de failover
	// 1. Arrêter la réplication sur le secondaire
	// 2. Promouvoir le secondaire en primaire
	// 3. Mettre à jour les services pour pointer vers le nouveau primaire
	// 4. Enregistrer l'événement de failover

	return ctrl.Result{}, nil
}

// handleRecovery gère la récupération après un failover
func (r *MariaDBHAReconciler) handleRecovery(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Exécution de la récupération")

	// TODO: Implémenter la logique de récupération
	// 1. Vérifier si l'ancien primaire est de nouveau disponible
	// 2. Reconfigurer l'ancien primaire comme nouveau secondaire
	// 3. Rétablir la réplication dans l'autre sens

	return ctrl.Result{}, nil
}

// SetupWithManager configure le contrôleur avec le Manager
func (r *MariaDBHAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("mariadbha-controller")
	
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1alpha1.MariaDBHA{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Complete(r)
}