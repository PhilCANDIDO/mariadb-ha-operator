// internal/controller/mariadbha_controller.go
package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

	// Suppression des StatefulSets
	primaryStsName := fmt.Sprintf("%s-primary", mariadbHA.Name)
	primarySts := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKey{Namespace: mariadbHA.Namespace, Name: primaryStsName}, primarySts)
	if err == nil {
		logger.Info("Suppression du StatefulSet primaire", "name", primaryStsName)
		if err := r.Delete(ctx, primarySts); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Impossible de supprimer le StatefulSet primaire")
			return ctrl.Result{}, err
		}
	}

	secondaryStsName := fmt.Sprintf("%s-secondary", mariadbHA.Name)
	secondarySts := &appsv1.StatefulSet{}
	err = r.Get(ctx, client.ObjectKey{Namespace: mariadbHA.Namespace, Name: secondaryStsName}, secondarySts)
	if err == nil {
		logger.Info("Suppression du StatefulSet secondaire", "name", secondaryStsName)
		if err := r.Delete(ctx, secondarySts); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Impossible de supprimer le StatefulSet secondaire")
			return ctrl.Result{}, err
		}
	}

	// Suppression des Services
	primarySvcName := fmt.Sprintf("%s-primary", mariadbHA.Name)
	primarySvc := &corev1.Service{}
	err = r.Get(ctx, client.ObjectKey{Namespace: mariadbHA.Namespace, Name: primarySvcName}, primarySvc)
	if err == nil {
		logger.Info("Suppression du Service primaire", "name", primarySvcName)
		if err := r.Delete(ctx, primarySvc); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Impossible de supprimer le Service primaire")
			return ctrl.Result{}, err
		}
	}

	secondarySvcName := fmt.Sprintf("%s-secondary", mariadbHA.Name)
	secondarySvc := &corev1.Service{}
	err = r.Get(ctx, client.ObjectKey{Namespace: mariadbHA.Namespace, Name: secondarySvcName}, secondarySvc)
	if err == nil {
		logger.Info("Suppression du Service secondaire", "name", secondarySvcName)
		if err := r.Delete(ctx, secondarySvc); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Impossible de supprimer le Service secondaire")
			return ctrl.Result{}, err
		}
	}

	clusterSvcName := fmt.Sprintf("%s-mariadb", mariadbHA.Name)
	clusterSvc := &corev1.Service{}
	err = r.Get(ctx, client.ObjectKey{Namespace: mariadbHA.Namespace, Name: clusterSvcName}, clusterSvc)
	if err == nil {
		logger.Info("Suppression du Service cluster", "name", clusterSvcName)
		if err := r.Delete(ctx, clusterSvc); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Impossible de supprimer le Service cluster")
			return ctrl.Result{}, err
		}
	}

	readOnlySvcName := fmt.Sprintf("%s-mariadb-ro", mariadbHA.Name)
	readOnlySvc := &corev1.Service{}
	err = r.Get(ctx, client.ObjectKey{Namespace: mariadbHA.Namespace, Name: readOnlySvcName}, readOnlySvc)
	if err == nil {
		logger.Info("Suppression du Service readonly", "name", readOnlySvcName)
		if err := r.Delete(ctx, readOnlySvc); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Impossible de supprimer le Service readonly")
			return ctrl.Result{}, err
		}
	}

	// Suppression des ConfigMaps
	primaryCfgName := fmt.Sprintf("%s-primary-config", mariadbHA.Name)
	primaryCfg := &corev1.ConfigMap{}
	err = r.Get(ctx, client.ObjectKey{Namespace: mariadbHA.Namespace, Name: primaryCfgName}, primaryCfg)
	if err == nil {
		logger.Info("Suppression du ConfigMap primaire", "name", primaryCfgName)
		if err := r.Delete(ctx, primaryCfg); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Impossible de supprimer le ConfigMap primaire")
			return ctrl.Result{}, err
		}
	}

	secondaryCfgName := fmt.Sprintf("%s-secondary-config", mariadbHA.Name)
	secondaryCfg := &corev1.ConfigMap{}
	err = r.Get(ctx, client.ObjectKey{Namespace: mariadbHA.Namespace, Name: secondaryCfgName}, secondaryCfg)
	if err == nil {
		logger.Info("Suppression du ConfigMap secondaire", "name", secondaryCfgName)
		if err := r.Delete(ctx, secondaryCfg); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Impossible de supprimer le ConfigMap secondaire")
			return ctrl.Result{}, err
		}
	}

	// Une fois tout nettoyé, retirer le finalizer
	controllerutil.RemoveFinalizer(mariadbHA, mariadbHAFinalizer)
	if err := r.Update(ctx, mariadbHA); err != nil {
		logger.Error(err, "Impossible de retirer le finalizer")
		return ctrl.Result{}, err
	}

	logger.Info("Finalizer retiré, ressource prête à être supprimée")
	return ctrl.Result{}, nil
}

// reconcileReplication configure la réplication entre primaire et secondaire
func (r *MariaDBHAReconciler) reconcileReplication(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Réconciliation de la réplication")

	// Vérifier si les deux instances sont prêtes
	primaryReady, err := r.isStatefulSetReady(ctx, mariadbHA, true)
	if err != nil {
		logger.Error(err, "Erreur lors de la vérification de l'état du StatefulSet primaire")
		return ctrl.Result{}, err
	}

	secondaryReady, err := r.isStatefulSetReady(ctx, mariadbHA, false)
	if err != nil {
		logger.Error(err, "Erreur lors de la vérification de l'état du StatefulSet secondaire")
		return ctrl.Result{}, err
	}

	if !primaryReady || !secondaryReady {
		logger.Info("Les StatefulSets ne sont pas encore prêts, report de la configuration de réplication")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Vérifier si la réplication est déjà configurée via le statut
	replicationConfigured := false
	for _, condition := range mariadbHA.Status.Conditions {
		if condition.Type == conditionReplicationActive && condition.Status == metav1.ConditionTrue {
			replicationConfigured = true
			break
		}
	}

	if replicationConfigured {
		logger.Info("La réplication est déjà configurée")
		return ctrl.Result{}, nil
	}

	// Récupérer les informations de connexion aux instances
	primaryInfo, err := getMariaDBConnectionInfo(ctx, r.Client, mariadbHA, true)
	if err != nil {
		logger.Error(err, "Impossible de récupérer les informations de connexion du primaire")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	secondaryInfo, err := getMariaDBConnectionInfo(ctx, r.Client, mariadbHA, false)
	if err != nil {
		logger.Error(err, "Impossible de récupérer les informations de connexion du secondaire")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Configurer la réplication
	err = r.setupReplication(ctx, mariadbHA, primaryInfo, secondaryInfo)
	if err != nil {
		logger.Error(err, "Échec de la configuration de la réplication")
		r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "ReplicationSetupFailed", 
			fmt.Sprintf("Échec de la configuration de la réplication: %s", err.Error()))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Mettre à jour la condition de réplication
	newCondition := metav1.Condition{
		Type:               conditionReplicationActive,
		Status:             metav1.ConditionTrue,
		Reason:             "ReplicationConfigured",
		Message:            "La réplication a été configurée avec succès",
		LastTransitionTime: metav1.Now(),
	}

	// Mettre à jour les conditions
	updated := false
	for i, condition := range mariadbHA.Status.Conditions {
		if condition.Type == conditionReplicationActive {
			mariadbHA.Status.Conditions[i] = newCondition
			updated = true
			break
		}
	}

	if !updated {
		mariadbHA.Status.Conditions = append(mariadbHA.Status.Conditions, newCondition)
	}

	if err := r.Status().Update(ctx, mariadbHA); err != nil {
		logger.Error(err, "Impossible de mettre à jour le statut de réplication")
		return ctrl.Result{}, err
	}

	r.Recorder.Event(mariadbHA, corev1.EventTypeNormal, "ReplicationConfigured", 
		"La réplication a été configurée avec succès")

	return ctrl.Result{}, nil
}

// setupReplication configure la réplication entre le primaire et le secondaire
func (r *MariaDBHAReconciler) setupReplication(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA, primaryInfo, secondaryInfo *MariaDBConnectionInfo) error {
	logger := log.FromContext(ctx)
	
	// Récupérer le nom d'utilisateur de réplication depuis la CR
	replicationUser := "replicator"
	if mariadbHA.Spec.Replication.User != "" {
		replicationUser = mariadbHA.Spec.Replication.User
	}
	
	// Récupérer le mot de passe de réplication depuis le secret
	secretName := ""
	if mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret != nil {
		secretName = mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret.Name
	}
	
	if secretName == "" {
		return fmt.Errorf("aucun secret spécifié pour les credentials de réplication")
	}
	
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: mariadbHA.Namespace, Name: secretName}, secret)
	if err != nil {
		return fmt.Errorf("impossible de récupérer le secret: %w", err)
	}
	
	replicationPassword := string(secret.Data["MARIADB_REPLICATION_PASSWORD"])
	if replicationPassword == "" {
		return fmt.Errorf("mot de passe de réplication non trouvé dans le secret")
	}
	
	// Se connecter au primaire pour obtenir la position actuelle du binlog
	primaryDB, err := connectToDatabase(primaryInfo)
	if err != nil {
		return fmt.Errorf("impossible de se connecter au primaire: %w", err)
	}
	defer primaryDB.Close()
	
	// Obtenir la position du binlog
	var binlogFile string
	var binlogPos int
	
	err = primaryDB.QueryRow("SHOW MASTER STATUS").Scan(&binlogFile, &binlogPos)
	if err != nil {
		return fmt.Errorf("impossible d'obtenir la position du binlog: %w", err)
	}
	
	logger.Info("Position du binlog obtenue", "file", binlogFile, "position", binlogPos)
	
	// Se connecter au secondaire pour configurer la réplication
	secondaryDB, err := connectToDatabase(secondaryInfo)
	if err != nil {
		return fmt.Errorf("impossible de se connecter au secondaire: %w", err)
	}
	defer secondaryDB.Close()
	
	// Arrêter la réplication si elle est déjà en cours
	_, err = secondaryDB.Exec("STOP SLAVE")
	if err != nil {
		// Essayer avec la nouvelle syntaxe
		_, err = secondaryDB.Exec("STOP REPLICA")
		if err != nil {
			logger.Info("Impossible d'arrêter la réplication, elle n'est probablement pas encore configurée")
		}
	}
	
	// Configurer la réplication
	replicationMode := "AFTER_GTID"
	if mariadbHA.Spec.Replication.Mode == "semisync" {
		// Activer la réplication semi-synchrone sur le primaire
		_, err = primaryDB.Exec("INSTALL PLUGIN rpl_semi_sync_master SONAME 'semisync_master.so'")
		if err != nil {
			logger.Info("Plugin rpl_semi_sync_master déjà installé ou erreur", "error", err)
		}
		
		_, err = primaryDB.Exec("SET GLOBAL rpl_semi_sync_master_enabled = 1")
		if err != nil {
			return fmt.Errorf("impossible d'activer la réplication semi-synchrone sur le primaire: %w", err)
		}
		
		// Activer la réplication semi-synchrone sur le secondaire
		_, err = secondaryDB.Exec("INSTALL PLUGIN rpl_semi_sync_slave SONAME 'semisync_slave.so'")
		if err != nil {
			logger.Info("Plugin rpl_semi_sync_slave déjà installé ou erreur", "error", err)
		}
		
		_, err = secondaryDB.Exec("SET GLOBAL rpl_semi_sync_slave_enabled = 1")
		if err != nil {
			return fmt.Errorf("impossible d'activer la réplication semi-synchrone sur le secondaire: %w", err)
		}
	}
	
	// Configurer la réplication
	query := fmt.Sprintf(
		"CHANGE MASTER TO "+
		"MASTER_HOST='%s', "+
		"MASTER_PORT=%d, "+
		"MASTER_USER='%s', "+
		"MASTER_PASSWORD='%s', "+
		"MASTER_LOG_FILE='%s', "+
		"MASTER_LOG_POS=%d",
		primaryInfo.Host,
		primaryInfo.Port,
		replicationUser,
		replicationPassword,
		binlogFile,
		binlogPos,
	)
	
	_, err = secondaryDB.Exec(query)
	if err != nil {
		return fmt.Errorf("impossible de configurer la réplication: %w", err)
	}
	
	// Démarrer la réplication
	_, err = secondaryDB.Exec("START SLAVE")
	if err != nil {
		// Essayer avec la nouvelle syntaxe
		_, err = secondaryDB.Exec("START REPLICA")
		if err != nil {
			return fmt.Errorf("impossible de démarrer la réplication: %w", err)
		}
	}
	
	logger.Info("Réplication configurée avec succès")
	return nil
}

// checkHealth surveille l'état de santé des instances MariaDB
func (r *MariaDBHAReconciler) checkHealth(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Vérification de l'état de santé du cluster")

	// Exécuter un health check complet
	healthCheckResult, err := r.performCompleteHealthCheck(ctx, mariadbHA)
	if err != nil {
		logger.Error(err, "Erreur lors du health check")
		return ctrl.Result{}, err
	}

	// Analyser les résultats du health check et prendre des décisions
	if !healthCheckResult.primaryHealthy {
		// Incrémenter le compteur d'échecs pour le primaire et vérifier le seuil
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

// SetupWithManager configure le contrôleur avec le Manager
func (r *MariaDBHAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("mariadbha-controller")
	
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1alpha1.MariaDBHA{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Complete(r)
}