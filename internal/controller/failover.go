// internal/controller/failover.go
package controller

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/PhilCANDIDO/mariadb-ha-operator/api/v1alpha1"
)

// handleFailover gère le processus de failover
func (r *MariaDBHAReconciler) handleFailover(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Exécution du failover")
	
	startTime := time.Now()

	// 1. Vérifier que le secondaire est en bon état avant la promotion
	secondaryHealth, _, err := r.checkInstanceHealth(ctx, mariadbHA, false)
	if err != nil || !secondaryHealth {
		logger.Error(err, "Le secondaire n'est pas en bon état pour le failover")
		r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverFailed", 
			"Échec du failover: le serveur secondaire n'est pas en bon état")
		
		// Marquer le cluster comme défaillant
		mariadbHA.Status.Phase = PhaseFailing
		if err := r.Status().Update(ctx, mariadbHA); err != nil {
			logger.Error(err, "Impossible de mettre à jour le statut après échec du failover")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 2. Récupérer les informations de connexion du secondaire
	secondaryInfo, err := getMariaDBConnectionInfo(ctx, r.Client, mariadbHA, false)
	if err != nil {
		logger.Error(err, "Impossible de récupérer les informations de connexion du secondaire")
		r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverFailed", 
			fmt.Sprintf("Échec du failover: impossible de se connecter au secondaire - %s", err.Error()))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 3. Vérifier l'état de la réplication et le lag si la stratégie est "safe"
	promotionStrategy := "safe" // Par défaut
	if mariadbHA.Spec.Failover.PromotionStrategy != "" {
		promotionStrategy = mariadbHA.Spec.Failover.PromotionStrategy
	}

	if promotionStrategy == "safe" {
		replicationStatus, err := checkReplicationStatus(secondaryInfo, 10*time.Second)
		if err != nil {
			logger.Error(err, "Impossible de vérifier l'état de la réplication sur le secondaire")
			
			if promotionStrategy == "safe" {
				// En mode "safe", on ne continue pas si on ne peut pas vérifier la réplication
				r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverFailed", 
					fmt.Sprintf("Échec du failover en mode safe: impossible de vérifier la réplication - %s", err.Error()))
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			
			// En mode "immediate", on émet juste un avertissement
			r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverWarning", 
				fmt.Sprintf("Le failover continue malgré l'impossibilité de vérifier la réplication: %s", err.Error()))
		} else if replicationStatus.SecondsBehindMaster != nil && *replicationStatus.SecondsBehindMaster > 0 {
			// Avertir sur le lag
			r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverLag", 
				fmt.Sprintf("Le secondaire a un retard de réplication de %d secondes", *replicationStatus.SecondsBehindMaster))
		}
	}

	// 4. Stopper la réplication sur le secondaire
	err = stopReplication(ctx, secondaryInfo)
	if err != nil {
		logger.Error(err, "Erreur lors de l'arrêt de la réplication sur le secondaire")
		r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverWarning", 
			fmt.Sprintf("Erreur lors de l'arrêt de la réplication: %s", err.Error()))
		// Continuer malgré l'erreur
	}

	// 5. Promouvoir le secondaire en nouveau primaire
	err = promoteSecondaryToPrimary(ctx, secondaryInfo, mariadbHA)
	if err != nil {
		logger.Error(err, "Erreur lors de la promotion du secondaire")
		r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverFailed", 
			fmt.Sprintf("Échec de la promotion du secondaire: %s", err.Error()))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 6. Mettre à jour les services pour pointer vers le nouveau primaire
	err = r.updateServicesAfterFailover(ctx, mariadbHA)
	if err != nil {
		logger.Error(err, "Erreur lors de la mise à jour des services")
		r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverWarning", 
			fmt.Sprintf("Services non mis à jour correctement: %s", err.Error()))
		// Continuer malgré l'erreur
	}

	// 7. Mettre à jour le statut de MariaDBHA
	now := metav1.Now()
	oldPrimary := mariadbHA.Status.CurrentPrimary
	newPrimary := mariadbHA.Status.CurrentSecondary

	// Calculer la durée du failover
	duration := int32(time.Since(startTime).Seconds())

	// Créer un événement de failover
	failoverEvent := databasev1alpha1.FailoverEvent{
		Timestamp:  now,
		Reason:     "PrimaryFailure",
		OldPrimary: oldPrimary,
		NewPrimary: newPrimary,
		Automatic:  true, // C'est un failover automatique
		Duration:   &duration,
	}

	// Mettre à jour le statut
	mariadbHA.Status.LastFailoverTime = &now
	mariadbHA.Status.CurrentPrimary = newPrimary
	mariadbHA.Status.CurrentSecondary = ""  // Temporairement vide jusqu'à ce qu'un nouveau secondaire soit configuré
	mariadbHA.Status.Phase = PhaseRecovering
	
	// Ajouter l'événement à l'historique
	if mariadbHA.Status.FailoverHistory == nil {
		mariadbHA.Status.FailoverHistory = []databasev1alpha1.FailoverEvent{failoverEvent}
	} else {
		mariadbHA.Status.FailoverHistory = append(mariadbHA.Status.FailoverHistory, failoverEvent)
	}

	// Réinitialiser les compteurs d'échec
	resetFailureCount(mariadbHA, true)
	resetFailureCount(mariadbHA, false)

	err = r.Status().Update(ctx, mariadbHA)
	if err != nil {
		logger.Error(err, "Erreur lors de la mise à jour du statut après failover")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Enregistrer un événement pour le failover réussi
	r.Recorder.Event(mariadbHA, corev1.EventTypeNormal, "FailoverSuccessful", 
		fmt.Sprintf("Failover réussi de %s vers %s en %d secondes", oldPrimary, newPrimary, duration))

	// Réenchaîner pour commencer la phase de récupération
	return ctrl.Result{Requeue: true}, nil
}

// stopReplication arrête la réplication sur une instance MariaDB
func stopReplication(ctx context.Context, info *MariaDBConnectionInfo) error {
	db, err := connectToDatabase(info)
	if err != nil {
		return err
	}
	defer db.Close()

	// Créer un contexte avec timeout
	execCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Arrêter la réplication (MariaDB 10.5+ utilise STOP REPLICA, avant c'était STOP SLAVE)
	_, err = db.ExecContext(execCtx, "STOP REPLICA")
	if err != nil {
		// Essayer avec l'ancienne syntaxe
		_, err = db.ExecContext(execCtx, "STOP SLAVE")
		if err != nil {
			return fmt.Errorf("impossible d'arrêter la réplication: %w", err)
		}
	}

	// Réinitialiser les informations de réplication
	_, err = db.ExecContext(execCtx, "RESET SLAVE ALL")
	if err != nil {
		// Essayer avec la nouvelle syntaxe
		_, err = db.ExecContext(execCtx, "RESET REPLICA ALL")
		if err != nil {
			return fmt.Errorf("impossible de réinitialiser la réplication: %w", err)
		}
	}

	return nil
}

// promoteSecondaryToPrimary promeut un serveur secondaire en primaire
func promoteSecondaryToPrimary(ctx context.Context, info *MariaDBConnectionInfo, mariadbHA *databasev1alpha1.MariaDBHA) error {
	db, err := connectToDatabase(info)
	if err != nil {
		return err
	}
	defer db.Close()

	// Créer un contexte avec timeout
	execCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Vérifier si la configuration demande de passer en mode read-only après failover
	readOnlyMode := false
	if mariadbHA.Spec.Failover.ReadOnlyMode != nil && *mariadbHA.Spec.Failover.ReadOnlyMode {
		readOnlyMode = true
	}

	// Configurer le serveur en mode lecture/écriture (ou read-only si spécifié)
	if readOnlyMode {
		_, err = db.ExecContext(execCtx, "SET GLOBAL read_only = ON")
		if err != nil {
			return fmt.Errorf("impossible de configurer le mode read-only: %w", err)
		}
	} else {
		_, err = db.ExecContext(execCtx, "SET GLOBAL read_only = OFF")
		if err != nil {
			return fmt.Errorf("impossible de désactiver le mode read-only: %w", err)
		}
	}

	// Éventuellement, exécuter des commandes supplémentaires pour la promotion
	// Par exemple, configurer un nouvel UUID de réplication si nécessaire

	return nil
}

// updateServicesAfterFailover met à jour les services Kubernetes pour pointer vers le nouveau primaire
func (r *MariaDBHAReconciler) updateServicesAfterFailover(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) error {
	logger := log.FromContext(ctx)

	// Récupérer le service pour le cluster MariaDB (celui qui pointe normalement vers le primaire)
	svcName := fmt.Sprintf("%s-mariadb", mariadbHA.Name)
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: mariadbHA.Namespace, Name: svcName}, svc)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Service principal non trouvé, rien à mettre à jour")
			return nil
		}
		return fmt.Errorf("impossible de récupérer le service principal: %w", err)
	}

	// Récupérer tous les labels du nouveau primaire
	podName := fmt.Sprintf("%s-secondary-0", mariadbHA.Name) // Ancien secondaire, maintenant primaire
	pod := &corev1.Pod{}
	err = r.Get(ctx, types.NamespacedName{Namespace: mariadbHA.Namespace, Name: podName}, pod)
	if err != nil {
		return fmt.Errorf("impossible de récupérer le pod du nouveau primaire: %w", err)
	}

	// Mettre à jour les sélecteurs du service pour pointer vers le nouveau primaire
	if svc.Spec.Selector == nil {
		svc.Spec.Selector = make(map[string]string)
	}

	// Mettre à jour le sélecteur pour qu'il corresponde au pod du nouveau primaire
	svc.Spec.Selector = pod.Labels

	// Mettre à jour le service
	err = r.Update(ctx, svc)
	if err != nil {
		return fmt.Errorf("impossible de mettre à jour le service principal: %w", err)
	}

	return nil
}

// handleRecovery gère la récupération après un failover
func (r *MariaDBHAReconciler) handleRecovery(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Gestion de la récupération après failover")

	// 1. Vérifier si l'ancien primaire est disponible
	oldPrimaryAvailable := false
	oldPrimaryName := ""

	// Trouver le nom de l'ancien primaire dans l'historique des failovers
	if len(mariadbHA.Status.FailoverHistory) > 0 {
		lastFailover := mariadbHA.Status.FailoverHistory[len(mariadbHA.Status.FailoverHistory)-1]
		oldPrimaryName = lastFailover.OldPrimary
	}

	if oldPrimaryName != "" {
		// Vérifier si le pod existe et est en état de fonctionnement
		pod := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Namespace: mariadbHA.Namespace, Name: oldPrimaryName}, pod)
		if err == nil && pod.Status.Phase == corev1.PodRunning {
			// Vérifier si MariaDB répond
			info, err := getMariaDBConnectionInfo(ctx, r.Client, mariadbHA, true) // Utiliser true car on vérifie l'ancien primaire
			if err == nil {
				oldPrimaryAvailable = checkTCPConnection(info, 5*time.Second)
			}
		}
	}

	// 2. Décider comment configurer le nouveau secondaire
	automaticFailback := false
	if mariadbHA.Spec.Failover.AutomaticFailback != nil {
		automaticFailback = *mariadbHA.Spec.Failover.AutomaticFailback
	}

	if oldPrimaryAvailable && automaticFailback {
		// Configurer l'ancien primaire comme nouveau secondaire
		err := r.configureAsSecondary(ctx, mariadbHA, oldPrimaryName)
		if err != nil {
			logger.Error(err, "Erreur lors de la configuration de l'ancien primaire comme secondaire")
			r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "RecoveryWarning", 
				fmt.Sprintf("Impossible de configurer l'ancien primaire comme secondaire: %s", err.Error()))
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Mettre à jour le statut
		mariadbHA.Status.CurrentSecondary = oldPrimaryName
		mariadbHA.Status.Phase = PhaseRunning
		err = r.Status().Update(ctx, mariadbHA)
		if err != nil {
			logger.Error(err, "Erreur lors de la mise à jour du statut après récupération")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		r.Recorder.Event(mariadbHA, corev1.EventTypeNormal, "RecoveryComplete", 
			fmt.Sprintf("Récupération terminée, %s configuré comme nouveau secondaire", oldPrimaryName))
	} else {
		// Créer un nouveau pod secondaire
		err := r.createNewSecondary(ctx, mariadbHA)
		if err != nil {
			logger.Error(err, "Erreur lors de la création d'un nouveau secondaire")
			r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "RecoveryWarning", 
				fmt.Sprintf("Impossible de créer un nouveau secondaire: %s", err.Error()))
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Le statut sera mis à jour lorsque le nouveau pod sera prêt
		// Pour l'instant, maintenir l'état "Recovering"
		r.Recorder.Event(mariadbHA, corev1.EventTypeNormal, "RecoveryInProgress", 
			"Création d'un nouveau secondaire en cours")
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// configureAsSecondary configure un serveur MariaDB comme un secondaire
func (r *MariaDBHAReconciler) configureAsSecondary(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA, podName string) error {
	// TODO: Implémenter la configuration complète du secondaire
	// 1. Configurer la réplication
	// 2. Mettre à jour les configurations
	// 3. Tester la réplication

	// Pour le moment, retourner nil pour simuler un succès
	return nil
}

// createNewSecondary crée un nouveau pod secondaire
func (r *MariaDBHAReconciler) createNewSecondary(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) error {
	// TODO: Implémenter la création d'un nouveau pod secondaire
	// 1. Créer un nouveau StatefulSet pour le secondaire
	// 2. Configurer la réplication
	// 3. Mettre à jour les services si nécessaire

	// Pour le moment, retourner nil pour simuler un succès
	return nil
}

// connectToDatabase établit une connexion à une base de données MariaDB
func connectToDatabase(info *MariaDBConnectionInfo) (*sql.DB, error) {
	// Construire le DSN
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", 
		info.Username, 
		info.Password, 
		info.Host, 
		info.Port, 
		info.Database)

	// Ouvrir la connexion
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("impossible d'ouvrir la connexion à la base de données: %w", err)
	}

	// Configurer la connexion
	db.SetConnMaxLifetime(30 * time.Second)
	db.SetMaxIdleConns(2)
	db.SetMaxOpenConns(5)

	// Test de la connexion
	err = db.Ping()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("impossible d'établir la connexion à la base de données: %w", err)
	}

	return db, nil
}

// ManualFailover déclenche un failover manuel
func (r *MariaDBHAReconciler) ManualFailover(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Déclenchement d'un failover manuel")
	
	// Vérifier si un failover est déjà en cours
	if mariadbHA.Status.Phase == PhaseFailover || mariadbHA.Status.Phase == PhaseRecovering {
		r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverRejected", 
			"Un failover est déjà en cours, impossible d'en démarrer un nouveau")
		return ctrl.Result{}, nil
	}
	
	// Vérifier le délai minimum entre failovers
	if mariadbHA.Status.LastFailoverTime != nil {
		minIntervalSeconds := int32(300) // 5 minutes par défaut
		if mariadbHA.Spec.Failover.MinimumIntervalSeconds != nil {
			minIntervalSeconds = *mariadbHA.Spec.Failover.MinimumIntervalSeconds
		}
		
		lastFailover := mariadbHA.Status.LastFailoverTime.Time
		minTimeBetweenFailovers := time.Duration(minIntervalSeconds) * time.Second
		if time.Since(lastFailover) < minTimeBetweenFailovers {
			r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverRejected", 
				fmt.Sprintf("Délai minimum entre failovers non écoulé (minimum: %s)", minTimeBetweenFailovers))
			return ctrl.Result{}, nil
		}
	}
	
	// Vérifier l'état du secondaire
	secondaryHealth, _, err := r.checkInstanceHealth(ctx, mariadbHA, false)
	if err != nil || !secondaryHealth {
		r.Recorder.Event(mariadbHA, corev1.EventTypeWarning, "FailoverRejected", 
			"Le serveur secondaire n'est pas en bon état pour un failover")
		return ctrl.Result{}, nil
	}
	
	// Tout est OK, déclencher le failover
	mariadbHA.Status.Phase = PhaseFailover
	r.Recorder.Event(mariadbHA, corev1.EventTypeNormal, "ManualFailoverStarted", 
		"Démarrage d'un failover manuel")
	
	err = r.Status().Update(ctx, mariadbHA)
	if err != nil {
		logger.Error(err, "Impossible de mettre à jour le statut pour le failover manuel")
		return ctrl.Result{}, err
	}
	
	// Réenchaîner pour traiter le failover
	return ctrl.Result{Requeue: true}, nil
}

// isFailoverAllowed vérifie si un failover peut être déclenché
func (r *MariaDBHAReconciler) isFailoverAllowed(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) bool {
	logger := log.FromContext(ctx)
	
	// Vérifier si le failover automatique est activé
	autoFailover := mariadbHA.Spec.Replication.AutomaticFailover != nil && *mariadbHA.Spec.Replication.AutomaticFailover
	if !autoFailover {
		logger.Info("Failover automatique désactivé")
		return false
	}
	
	// Vérifier si un failover est déjà en cours
	if mariadbHA.Status.Phase == PhaseFailover || mariadbHA.Status.Phase == PhaseRecovering {
		logger.Info("Un failover ou une récupération est déjà en cours")
		return false
	}
	
	// Vérifier le délai minimum entre failovers
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
			return false
		}
	}
	
	// Tout est OK pour un failover
	return true
}