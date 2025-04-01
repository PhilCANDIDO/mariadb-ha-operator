// internal/controller/health_check.go
package controller

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/PhilCANDIDO/mariadb-ha-operator/api/v1alpha1"
)

// MariaDBConnectionInfo contient les informations de connexion à une instance MariaDB
type MariaDBConnectionInfo struct {
	Host     string
	Port     int
	Username string
	Password string
	Database string
}

// getMariaDBConnectionInfo récupère les informations de connexion à partir de la CR et des secrets
func getMariaDBConnectionInfo(ctx context.Context, c client.Client, mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool) (*MariaDBConnectionInfo, error) {
	logger := log.FromContext(ctx)

	// Construire le nom du pod selon qu'il s'agit du primaire ou du secondaire
	podName := fmt.Sprintf("%s-primary-0", mariadbHA.Name)
	if !isPrimary {
		podName = fmt.Sprintf("%s-secondary-0", mariadbHA.Name)
	}

	// Récupérer le secret contenant les informations d'authentification
	var secretName string
	var usernameKey, passwordKey string

	// Chercher d'abord dans la config de l'instance
	instanceConfig := mariadbHA.Spec.Instances.Primary.Config
	if !isPrimary {
		instanceConfig = mariadbHA.Spec.Instances.Secondary.Config
	}

	if instanceConfig.UserCredentialsSecret != nil {
		secretName = instanceConfig.UserCredentialsSecret.Name
		if instanceConfig.UserCredentialsSecret.Keys != nil {
			usernameKey = instanceConfig.UserCredentialsSecret.Keys["username"]
			passwordKey = instanceConfig.UserCredentialsSecret.Keys["password"]
		}
	} else if mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret != nil {
		// Sinon, utiliser la config commune
		secretName = mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret.Name
		if mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret.Keys != nil {
			usernameKey = mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret.Keys["username"]
			passwordKey = mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret.Keys["password"]
		}
	}

	// Valeurs par défaut pour les clés
	if usernameKey == "" {
		usernameKey = "MARIADB_USER"
	}
	if passwordKey == "" {
		passwordKey = "MARIADB_PASSWORD"
	}

	// Récupérer le secret
	if secretName == "" {
		return nil, fmt.Errorf("aucun secret spécifié pour les credentials MariaDB")
	}

	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: mariadbHA.Namespace, Name: secretName}, secret)
	if err != nil {
		logger.Error(err, "Impossible de récupérer le secret d'authentification", "secretName", secretName)
		return nil, err
	}

	username := string(secret.Data[usernameKey])
	password := string(secret.Data[passwordKey])

	// Récupérer le pod pour obtenir l'IP
	pod := &corev1.Pod{}
	err = c.Get(ctx, types.NamespacedName{Namespace: mariadbHA.Namespace, Name: podName}, pod)
	if err != nil {
		logger.Error(err, "Impossible de récupérer le pod", "podName", podName)
		return nil, err
	}

	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("le pod %s n'a pas encore d'IP", podName)
	}

	// Par défaut, utiliser le port 3306
	port := 3306

	// Créer et retourner les infos de connexion
	return &MariaDBConnectionInfo{
		Host:     pod.Status.PodIP,
		Port:     port,
		Username: username,
		Password: password,
		Database: "mysql", // Base de données par défaut pour les health checks
	}, nil
}

// checkTCPConnection vérifie si on peut établir une connexion TCP avec l'instance MariaDB
func checkTCPConnection(info *MariaDBConnectionInfo, timeout time.Duration) bool {
	address := fmt.Sprintf("%s:%d", info.Host, info.Port)
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	return true
}

// checkSQLConnection vérifie si on peut exécuter une requête SQL simple
func checkSQLConnection(info *MariaDBConnectionInfo, timeout time.Duration) bool {
	// Construire le DSN
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", 
		info.Username, 
		info.Password, 
		info.Host, 
		info.Port, 
		info.Database)

	// Ouvrir la connexion avec un timeout
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return false
	}
	defer db.Close()

	// Configurer le timeout
	db.SetConnMaxLifetime(timeout)
	db.SetMaxIdleConns(1)
	db.SetMaxOpenConns(1)

	// Ping la base pour vérifier la connexion
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	
	err = db.PingContext(ctx)
	if err != nil {
		return false
	}

	// Exécuter une requête simple
	var result int
	row := db.QueryRowContext(ctx, "SELECT 1")
	err = row.Scan(&result)
	
	return err == nil && result == 1
}

// ReplicationStatus contient l'état de la réplication
type ReplicationStatus struct {
	SlaveRunning        bool
	LastIOErrno         int
	LastSQLErrno        int
	SecondsBehindMaster *int32
	IORunning           string
	SQLRunning          string
}

// checkReplicationStatus vérifie l'état de la réplication sur le secondaire
func checkReplicationStatus(info *MariaDBConnectionInfo, timeout time.Duration) (*ReplicationStatus, error) {
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
		return nil, err
	}
	defer db.Close()

	// Configurer le timeout
	db.SetConnMaxLifetime(timeout)
	db.SetMaxIdleConns(1)
	db.SetMaxOpenConns(1)

	// Créer un contexte avec timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Exécuter SHOW REPLICA STATUS (MariaDB 10.5+) ou SHOW SLAVE STATUS (MariaDB < 10.5)
	var rows *sql.Rows
	rows, err = db.QueryContext(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		// Si la commande échoue, essayer avec SHOW SLAVE STATUS
		rows, err = db.QueryContext(ctx, "SHOW SLAVE STATUS")
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()

	// Vérifier si la réplication est configurée
	if !rows.Next() {
		return nil, fmt.Errorf("pas de réplication configurée")
	}

	// Récupérer toutes les colonnes
	columns, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}

	// Créer une map pour stocker les données de la ligne
	values := make([]interface{}, len(columns))
	for i := range values {
		values[i] = new(sql.RawBytes)
	}
	
	// Lire les valeurs
	if err := rows.Scan(values...); err != nil {
		return nil, err
	}

	// Créer une map pour un accès facile par nom de colonne
	data := make(map[string]string)
	for i, column := range columns {
		data[column.Name()] = string(*values[i].(*sql.RawBytes))
	}

	// Créer l'objet de statut de réplication
	status := &ReplicationStatus{
		SlaveRunning: data["Slave_IO_Running"] == "Yes" && data["Slave_SQL_Running"] == "Yes",
		IORunning:    data["Slave_IO_Running"],
		SQLRunning:   data["Slave_SQL_Running"],
	}

	// Convertir les erreurs en nombres
	if v, ok := data["Last_IO_Errno"]; ok && v != "" {
		status.LastIOErrno, _ = strconv.Atoi(v)
	}
	if v, ok := data["Last_SQL_Errno"]; ok && v != "" {
		status.LastSQLErrno, _ = strconv.Atoi(v)
	}

	// Convertir le délai de réplication
	if v, ok := data["Seconds_Behind_Master"]; ok && v != "" && v != "NULL" {
		seconds, err := strconv.Atoi(v)
		if err == nil {
			sec32 := int32(seconds)
			status.SecondsBehindMaster = &sec32
		}
	}

	return status, nil
}

// executeHealthChecks exécute tous les health checks définis pour une instance MariaDB
func executeHealthChecks(ctx context.Context, c client.Client, mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool) (bool, *int32, error) {
	logger := log.FromContext(ctx)
	
	// Déterminer les health checks à effectuer et leurs timeouts
	timeoutSeconds := int32(2) // Par défaut 2 secondes
	if mariadbHA.Spec.Failover.FailureDetection.TimeoutSeconds != nil {
		timeoutSeconds = *mariadbHA.Spec.Failover.FailureDetection.TimeoutSeconds
	}
	timeout := time.Duration(timeoutSeconds) * time.Second

	// Récupérer les informations de connexion
	info, err := getMariaDBConnectionInfo(ctx, c, mariadbHA, isPrimary)
	if err != nil {
		logger.Error(err, "Impossible de récupérer les informations de connexion")
		return false, nil, err
	}

	// Vérifier la connexion TCP
	if !checkTCPConnection(info, timeout) {
		logger.Info("Health check TCP échoué", "host", info.Host, "port", info.Port)
		return false, nil, nil
	}

	// Vérifier la connexion SQL
	if !checkSQLConnection(info, timeout) {
		logger.Info("Health check SQL échoué", "host", info.Host)
		return false, nil, nil
	}

	// Si c'est l'instance secondaire, vérifier la réplication
	if !isPrimary {
		status, err := checkReplicationStatus(info, timeout)
		if err != nil {
			logger.Error(err, "Impossible de vérifier l'état de la réplication")
			return true, nil, err // Le serveur est en vie mais la réplication n'est pas vérifiable
		}

		// Vérifier l'état de la réplication
		if !status.SlaveRunning {
			logger.Info("Réplication inactive", 
				"IO_Running", status.IORunning, 
				"SQL_Running", status.SQLRunning,
				"IO_Errno", status.LastIOErrno,
				"SQL_Errno", status.LastSQLErrno)
			return true, status.SecondsBehindMaster, nil // Le serveur est en vie mais la réplication est inactive
		}

		// Vérifier le lag de réplication
		maxLagSeconds := int32(30) // Par défaut 30 secondes
		if mariadbHA.Spec.Replication.MaxLagSeconds != nil {
			maxLagSeconds = *mariadbHA.Spec.Replication.MaxLagSeconds
		}

		if status.SecondsBehindMaster != nil && *status.SecondsBehindMaster > maxLagSeconds {
			logger.Info("Lag de réplication trop important", 
				"seconds_behind_master", *status.SecondsBehindMaster,
				"max_lag_seconds", maxLagSeconds)
			return true, status.SecondsBehindMaster, nil // Le serveur est en vie mais le lag est trop important
		}

		// Tout est OK pour le secondaire avec la réplication
		return true, status.SecondsBehindMaster, nil
	}

	// Tout est OK pour le primaire
	return true, nil, nil
}

// checkInstanceHealth vérifie l'état de santé d'une instance (primaire ou secondaire)
func (r *MariaDBHAReconciler) checkInstanceHealth(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool) (bool, *int32, error) {
	logger := log.FromContext(ctx)
	
	instanceType := "primaire"
	if !isPrimary {
		instanceType = "secondaire"
	}
	
	logger.Info("Vérification de l'état de santé de l'instance", "type", instanceType)
	
	// Exécuter les health checks
	isHealthy, replicationLag, err := executeHealthChecks(ctx, r.Client, mariadbHA, isPrimary)
	if err != nil {
		logger.Error(err, "Erreur lors des health checks", "type", instanceType)
		// On considère que l'erreur non fatale = instance non saine
		return false, replicationLag, nil
	}
	
	// Mettre à jour les conditions dans le statut
	condition := conditionPrimaryReady
	if !isPrimary {
		condition = conditionSecondaryReady
	}
	
	// Mettre à jour la condition
	updateCondition := func() error {
		status := metav1.ConditionTrue
		reason := "InstanceHealthy"
		message := fmt.Sprintf("L'instance %s est en bon état", instanceType)
		
		if !isHealthy {
			status = metav1.ConditionFalse
			reason = "InstanceUnhealthy"
			message = fmt.Sprintf("L'instance %s n'est pas en bon état", instanceType)
		}
		
		// Ne pas mettre à jour si la condition n'a pas changé
		for i, c := range mariadbHA.Status.Conditions {
			if c.Type == condition && c.Status == status {
				return nil
			} else if c.Type == condition {
				// Mettre à jour la condition existante
				mariadbHA.Status.Conditions[i].Status = status
				mariadbHA.Status.Conditions[i].Reason = reason
				mariadbHA.Status.Conditions[i].Message = message
				mariadbHA.Status.Conditions[i].LastTransitionTime = metav1.Now()
				return r.Status().Update(ctx, mariadbHA)
			}
		}
		
		// Ajouter une nouvelle condition
		newCondition := metav1.Condition{
			Type:               condition,
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		}
		mariadbHA.Status.Conditions = append(mariadbHA.Status.Conditions, newCondition)
		return r.Status().Update(ctx, mariadbHA)
	}
	
	if err := updateCondition(); err != nil {
		logger.Error(err, "Impossible de mettre à jour la condition", "condition", condition)
	}
	
	return isHealthy, replicationLag, nil
}

// performCompleteHealthCheck exécute un health check complet du cluster MariaDB HA
func (r *MariaDBHAReconciler) performCompleteHealthCheck(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (HealthCheckResult, error) {
	logger := log.FromContext(ctx)
	logger.Info("Exécution d'un health check complet")
	
	result := HealthCheckResult{
		primaryHealthy:    false,
		secondaryHealthy:  false,
		replicationHealthy: false,
	}
	
	// Vérifier l'instance primaire
	primaryHealthy, _, err := r.checkInstanceHealth(ctx, mariadbHA, true)
	if err != nil {
		logger.Error(err, "Erreur lors du health check du primaire")
		return result, err
	}
	result.primaryHealthy = primaryHealthy
	
	// Vérifier l'instance secondaire
	secondaryHealthy, replicationLag, err := r.checkInstanceHealth(ctx, mariadbHA, false)
	if err != nil {
		logger.Error(err, "Erreur lors du health check du secondaire")
		return result, err
	}
	result.secondaryHealthy = secondaryHealthy
	result.replicationLag = replicationLag
	
	// Mettre à jour la condition de réplication
	replicationActive := secondaryHealthy && replicationLag != nil
	
	// Mettre à jour la condition
	updateReplicationCondition := func() error {
		status := metav1.ConditionTrue
		reason := "ReplicationActive"
		message := "La réplication est active"
		
		if !replicationActive {
			status = metav1.ConditionFalse
			reason = "ReplicationInactive"
			message = "La réplication n'est pas active"
		}
		
		// Ne pas mettre à jour si la condition n'a pas changé
		for i, c := range mariadbHA.Status.Conditions {
			if c.Type == conditionReplicationActive && c.Status == status {
				return nil
			} else if c.Type == conditionReplicationActive {
				// Mettre à jour la condition existante
				mariadbHA.Status.Conditions[i].Status = status
				mariadbHA.Status.Conditions[i].Reason = reason
				mariadbHA.Status.Conditions[i].Message = message
				mariadbHA.Status.Conditions[i].LastTransitionTime = metav1.Now()
				return r.Status().Update(ctx, mariadbHA)
			}
		}
		
		// Ajouter une nouvelle condition
		newCondition := metav1.Condition{
			Type:               conditionReplicationActive,
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		}
		mariadbHA.Status.Conditions = append(mariadbHA.Status.Conditions, newCondition)
		return r.Status().Update(ctx, mariadbHA)
	}
	
	if err := updateReplicationCondition(); err != nil {
		logger.Error(err, "Impossible de mettre à jour la condition de réplication")
	}
	
	result.replicationHealthy = replicationActive
	
	// Compter les échecs précédents
	if !result.primaryHealthy {
		result.primaryFailCount = getFailureCount(mariadbHA, true)
		result.primaryFailCount++
	}
	
	if !result.secondaryHealthy {
		result.secondaryFailCount = getFailureCount(mariadbHA, false)
		result.secondaryFailCount++
	}
	
	// Sauvegarder les compteurs d'échec
	saveFailureCount(mariadbHA, true, result.primaryFailCount)
	saveFailureCount(mariadbHA, false, result.secondaryFailCount)
	
	return result, nil
}

// FailureCountAnnotation définit les annotations pour suivre les compteurs d'échec
const (
	PrimaryFailCountAnnotation   = "database.cpf-it.fr/primary-fail-count"
	SecondaryFailCountAnnotation = "database.cpf-it.fr/secondary-fail-count"
)

// getFailureCount récupère le compteur d'échecs à partir des annotations
func getFailureCount(mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool) int {
	if mariadbHA.Annotations == nil {
		return 0
	}
	
	annotation := PrimaryFailCountAnnotation
	if !isPrimary {
		annotation = SecondaryFailCountAnnotation
	}
	
	countStr, exists := mariadbHA.Annotations[annotation]
	if !exists {
		return 0
	}
	
	count, err := strconv.Atoi(countStr)
	if err != nil {
		return 0
	}
	
	return count
}

// saveFailureCount sauvegarde le compteur d'échecs dans les annotations
func saveFailureCount(mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool, count int) {
	if mariadbHA.Annotations == nil {
		mariadbHA.Annotations = make(map[string]string)
	}
	
	annotation := PrimaryFailCountAnnotation
	if !isPrimary {
		annotation = SecondaryFailCountAnnotation
	}
	
	if count > 0 {
		mariadbHA.Annotations[annotation] = strconv.Itoa(count)
	} else {
		delete(mariadbHA.Annotations, annotation)
	}
}

// resetFailureCount réinitialise le compteur d'échecs
func resetFailureCount(mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool) {
	if mariadbHA.Annotations == nil {
		return
	}
	
	annotation := PrimaryFailCountAnnotation
	if !isPrimary {
		annotation = SecondaryFailCountAnnotation
	}
	
	delete(mariadbHA.Annotations, annotation)
}