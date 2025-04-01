// internal/controller/statefulset.go
package controller

import (
	"context"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/PhilCANDIDO/mariadb-ha-operator/api/v1alpha1"
)

// reconcileStatefulSets crée ou met à jour les StatefulSets pour les instances primaire et secondaire
func (r *MariaDBHAReconciler) reconcileStatefulSets(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Réconciliation des StatefulSets")

	// Réconcilier le StatefulSet du primaire
	result, err := r.reconcilePrimaryStatefulSet(ctx, mariadbHA)
	if err != nil || result.Requeue {
		return result, err
	}

	// Réconcilier le StatefulSet du secondaire
	result, err = r.reconcileSecondaryStatefulSet(ctx, mariadbHA)
	if err != nil || result.Requeue {
		return result, err
	}

	// Vérifier si les StatefulSets sont prêts
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

	// Mettre à jour le statut si les deux StatefulSets sont prêts et que le cluster est en initialisation
	if primaryReady && secondaryReady && mariadbHA.Status.Phase == PhaseInitializing {
		logger.Info("Les deux StatefulSets sont prêts, le cluster passe en phase Running")
		mariadbHA.Status.Phase = PhaseRunning
		if err := r.Status().Update(ctx, mariadbHA); err != nil {
			logger.Error(err, "Impossible de mettre à jour le statut")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// reconcilePrimaryStatefulSet crée ou met à jour le StatefulSet pour l'instance primaire
func (r *MariaDBHAReconciler) reconcilePrimaryStatefulSet(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	return r.reconcileInstanceStatefulSet(ctx, mariadbHA, true)
}

// reconcileSecondaryStatefulSet crée ou met à jour le StatefulSet pour l'instance secondaire
func (r *MariaDBHAReconciler) reconcileSecondaryStatefulSet(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	return r.reconcileInstanceStatefulSet(ctx, mariadbHA, false)
}

// reconcileInstanceStatefulSet crée ou met à jour un StatefulSet pour une instance primaire ou secondaire
func (r *MariaDBHAReconciler) reconcileInstanceStatefulSet(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Déterminer le nom et la configuration selon le type d'instance
	instanceType := "primary"
	if !isPrimary {
		instanceType = "secondary"
	}

	name := fmt.Sprintf("%s-%s", mariadbHA.Name, instanceType)
	instanceConfig := mariadbHA.Spec.Instances.Primary
	if !isPrimary {
		instanceConfig = mariadbHA.Spec.Instances.Secondary
	}

	// Fusion des configurations commune et spécifique
	mergedConfig := mergeMySQLConfigs(mariadbHA.Spec.Instances.CommonConfig, instanceConfig.Config)

	// Récupérer le StatefulSet existant s'il existe
	found := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: mariadbHA.Namespace}, found)
	
	// Créer un nouveau StatefulSet s'il n'existe pas
	if err != nil && errors.IsNotFound(err) {
		// Créer le ConfigMap pour la configuration MariaDB
		configMap, err := r.createOrUpdateConfigMap(ctx, mariadbHA, isPrimary, mergedConfig)
		if err != nil {
			logger.Error(err, "Impossible de créer le ConfigMap de configuration")
			return ctrl.Result{}, err
		}

		// Créer le StatefulSet
		sts := r.buildStatefulSet(mariadbHA, isPrimary, instanceConfig, configMap.Name)
		logger.Info("Création du StatefulSet", "name", name)
		
		// Définir la référence pour que le StatefulSet soit supprimé avec le MariaDBHA
		if err := controllerutil.SetControllerReference(mariadbHA, sts, r.Scheme); err != nil {
			logger.Error(err, "Impossible de définir la référence du contrôleur")
			return ctrl.Result{}, err
		}
		
		if err := r.Create(ctx, sts); err != nil {
			logger.Error(err, "Impossible de créer le StatefulSet")
			return ctrl.Result{}, err
		}
		
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		logger.Error(err, "Erreur lors de la récupération du StatefulSet")
		return ctrl.Result{}, err
	}

	// Le StatefulSet existe, vérifier s'il doit être mis à jour
	// Créer le ConfigMap pour la configuration MariaDB
	configMap, err := r.createOrUpdateConfigMap(ctx, mariadbHA, isPrimary, mergedConfig)
	if err != nil {
		logger.Error(err, "Impossible de mettre à jour le ConfigMap de configuration")
		return ctrl.Result{}, err
	}

	// Construire le StatefulSet souhaité pour comparaison
	desired := r.buildStatefulSet(mariadbHA, isPrimary, instanceConfig, configMap.Name)
	
	// Mettre à jour le StatefulSet si nécessaire
	if needsUpdate(found, desired) {
		logger.Info("Mise à jour du StatefulSet", "name", name)
		
		// Préserver certains champs qui ne doivent pas être écrasés
		desired.Spec.VolumeClaimTemplates = found.Spec.VolumeClaimTemplates
		desired.ResourceVersion = found.ResourceVersion
		
		if err := r.Update(ctx, desired); err != nil {
			logger.Error(err, "Impossible de mettre à jour le StatefulSet")
			return ctrl.Result{}, err
		}
		
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

// buildStatefulSet construit un StatefulSet pour une instance MariaDB
func (r *MariaDBHAReconciler) buildStatefulSet(mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool, instanceConfig databasev1alpha1.InstanceConfig, configMapName string) *appsv1.StatefulSet {
	// Déterminer le nom et les labels
	instanceType := "primary"
	if !isPrimary {
		instanceType = "secondary"
	}
	name := fmt.Sprintf("%s-%s", mariadbHA.Name, instanceType)
	
	labels := map[string]string{
		"app": "mariadb",
		"mariadb.cpf-it.fr/cluster": mariadbHA.Name,
		"mariadb.cpf-it.fr/role": instanceType,
	}
	
	// Fusionner avec les annotations de pod définies dans la CR
	podAnnotations := make(map[string]string)
	if instanceConfig.PodAnnotations != nil {
		for k, v := range instanceConfig.PodAnnotations {
			podAnnotations[k] = v
		}
	}
	
	// Déterminer l'image MariaDB à utiliser
	image := "mariadb:10.5"
	if instanceConfig.Config.Image != "" {
		image = instanceConfig.Config.Image
	} else if mariadbHA.Spec.Instances.CommonConfig.Image != "" {
		image = mariadbHA.Spec.Instances.CommonConfig.Image
	}
	
	// Construire les volumes
	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		},
		{
			Name: "init-scripts",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		},
	}
	
	// Construire les montages de volumes
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "data",
			MountPath: "/var/lib/mysql",
		},
		{
			Name:      "config",
			MountPath: "/etc/mysql/conf.d",
		},
		{
			Name:      "init-scripts",
			MountPath: "/docker-entrypoint-initdb.d",
		},
	}
	
	// Récupérer le nom du secret pour les credentials
	secretName := ""
	if instanceConfig.Config.UserCredentialsSecret != nil {
		secretName = instanceConfig.Config.UserCredentialsSecret.Name
	} else if mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret != nil {
		secretName = mariadbHA.Spec.Instances.CommonConfig.UserCredentialsSecret.Name
	}
	
	// Construire les variables d'environnement
	env := []corev1.EnvVar{}
	if secretName != "" {
		// Ajouter les références aux secrets pour les credentials
		env = append(env, corev1.EnvVar{
			Name: "MARIADB_ROOT_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: secretName,
					},
					Key: "MARIADB_ROOT_PASSWORD",
				},
			},
		})
		
		// Ajouter d'autres variables d'environnement selon les besoins
		// Pour un utilisateur normal
		env = append(env, corev1.EnvVar{
			Name: "MARIADB_USER",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: secretName,
					},
					Key: "MARIADB_USER",
					Optional: boolPtr(true),
				},
			},
		})
		
		env = append(env, corev1.EnvVar{
			Name: "MARIADB_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: secretName,
					},
					Key: "MARIADB_PASSWORD",
					Optional: boolPtr(true),
				},
			},
		})
		
		// Pour la base de données
		env = append(env, corev1.EnvVar{
			Name: "MARIADB_DATABASE",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: secretName,
					},
					Key: "MARIADB_DATABASE",
					Optional: boolPtr(true),
				},
			},
		})
	}
	
	// Ajouter le rôle comme variable d'environnement
	env = append(env, corev1.EnvVar{
		Name:  "MARIADB_ROLE",
		Value: instanceType,
	})
	
	// Construire le probe de disponibilité (readiness)
	readinessProbe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"mysqladmin",
					"ping",
					"-h",
					"localhost",
					"-u",
					"root",
					"-p${MARIADB_ROOT_PASSWORD}",
				},
			},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}
	
	// Construire le probe de vivacité (liveness)
	livenessProbe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"mysqladmin",
					"ping",
					"-h",
					"localhost",
					"-u",
					"root",
					"-p${MARIADB_ROOT_PASSWORD}",
				},
			},
		},
		InitialDelaySeconds: 60,
		PeriodSeconds:       20,
		TimeoutSeconds:      5,
		SuccessThreshold:    1,
		FailureThreshold:    6,
	}
	
	// Construire le StatefulSet
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mariadbHA.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1), // Toujours 1 replica par instance
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			ServiceName: name, // Service headless
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "mariadb",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports: []corev1.ContainerPort{
								{
									Name:          "mysql",
									ContainerPort: 3306,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Env:             env,
							Resources:       instanceConfig.Resources,
							VolumeMounts:    volumeMounts,
							ReadinessProbe:  readinessProbe,
							LivenessProbe:   livenessProbe,
						},
					},
					Volumes: volumes,
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: getAccessModes(instanceConfig.Storage.AccessModes),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(instanceConfig.Storage.Size),
							},
						},
						StorageClassName: instanceConfig.Storage.StorageClassName,
					},
				},
			},
		},
	}
	
	// Ajouter les nodeSelector et toleration si définis
	if instanceConfig.NodeSelector != nil {
		sts.Spec.Template.Spec.NodeSelector = instanceConfig.NodeSelector
	}
	
	if instanceConfig.Tolerations != nil {
		sts.Spec.Template.Spec.Tolerations = instanceConfig.Tolerations
	}
	
	return sts
}

// createOrUpdateConfigMap crée ou met à jour le ConfigMap pour la configuration MariaDB
func (r *MariaDBHAReconciler) createOrUpdateConfigMap(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool, config databasev1alpha1.MariaDBConfig) (*corev1.ConfigMap, error) {
	logger := log.FromContext(ctx)
	
	// Déterminer le nom et le type d'instance
	instanceType := "primary"
	if !isPrimary {
		instanceType = "secondary"
	}
	name := fmt.Sprintf("%s-%s-config", mariadbHA.Name, instanceType)
	
	// Générer le contenu du fichier de configuration
	configContent := generateMariaDBConfig(isPrimary, config.ConfigurationParameters)
	
	// Générer le script d'initialisation
	initScript := generateInitScript(isPrimary, mariadbHA)
	
	// Créer le ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mariadbHA.Namespace,
			Labels: map[string]string{
				"app": "mariadb",
				"mariadb.cpf-it.fr/cluster": mariadbHA.Name,
				"mariadb.cpf-it.fr/role": instanceType,
			},
		},
		Data: map[string]string{
			"mariadb.cnf": configContent,
			"init.sql": initScript,
		},
	}
	
	// Définir la référence pour que le ConfigMap soit supprimé avec le MariaDBHA
	if err := controllerutil.SetControllerReference(mariadbHA, cm, r.Scheme); err != nil {
		logger.Error(err, "Impossible de définir la référence du contrôleur pour le ConfigMap")
		return nil, err
	}
	
	// Récupérer le ConfigMap existant s'il existe
	found := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: mariadbHA.Namespace}, found)
	
	// Créer un nouveau ConfigMap s'il n'existe pas
	if err != nil && errors.IsNotFound(err) {
		logger.Info("Création du ConfigMap", "name", name)
		if err := r.Create(ctx, cm); err != nil {
			logger.Error(err, "Impossible de créer le ConfigMap")
			return nil, err
		}
		return cm, nil
	} else if err != nil {
		logger.Error(err, "Erreur lors de la récupération du ConfigMap")
		return nil, err
	}
	
	// Mettre à jour le ConfigMap si nécessaire
	if found.Data["mariadb.cnf"] != configContent || found.Data["init.sql"] != initScript {
		logger.Info("Mise à jour du ConfigMap", "name", name)
		found.Data = cm.Data
		if err := r.Update(ctx, found); err != nil {
			logger.Error(err, "Impossible de mettre à jour le ConfigMap")
			return nil, err
		}
	}
	
	return found, nil
}

// generateMariaDBConfig génère le contenu du fichier de configuration MariaDB
func generateMariaDBConfig(isPrimary bool, params map[string]string) string {
	config := "[mysqld]\n"
	
	// Paramètres de base
	config += "character-set-server = utf8mb4\n"
	config += "collation-server = utf8mb4_unicode_ci\n"
	
	// Paramètres spécifiques au rôle
	if isPrimary {
		config += "# Configuration du serveur primaire\n"
		config += "log_bin = mysql-bin\n"
		config += "server_id = 1\n"
		// Paramètres de performance et de durabilité pour le primaire
		if _, ok := params["innodb_flush_log_at_trx_commit"]; !ok {
			config += "innodb_flush_log_at_trx_commit = 1\n"
		}
		if _, ok := params["sync_binlog"]; !ok {
			config += "sync_binlog = 1\n"
		}
	} else {
		config += "# Configuration du serveur secondaire\n"
		config += "server_id = 2\n"
		config += "read_only = ON\n"
		// Paramètres de performance pour le secondaire
		if _, ok := params["innodb_flush_log_at_trx_commit"]; !ok {
			config += "innodb_flush_log_at_trx_commit = 2\n"
		}
		if _, ok := params["sync_binlog"]; !ok {
			config += "sync_binlog = 0\n"
		}
	}
	
	// Ajouter tous les paramètres personnalisés
	for k, v := range params {
		config += fmt.Sprintf("%s = %s\n", k, v)
	}
	
	return config
}

// generateInitScript génère le script d'initialisation pour MariaDB
func generateInitScript(isPrimary bool, mariadbHA *databasev1alpha1.MariaDBHA) string {
	script := "-- Script d'initialisation MariaDB\n"
	
	// Créer l'utilisateur de réplication si nécessaire
	if isPrimary {
		replicationUser := "replicator"
		if mariadbHA.Spec.Replication.User != "" {
			replicationUser = mariadbHA.Spec.Replication.User
		}
		
		script += fmt.Sprintf("-- Création de l'utilisateur de réplication\n")
		script += fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '${MARIADB_REPLICATION_PASSWORD}';\n", replicationUser)
		script += fmt.Sprintf("GRANT REPLICATION SLAVE ON *.* TO '%s'@'%%';\n", replicationUser)
		script += "FLUSH PRIVILEGES;\n"
	}
	
	return script
}

// isStatefulSetReady vérifie si un StatefulSet est prêt
func (r *MariaDBHAReconciler) isStatefulSetReady(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool) (bool, error) {
	// Déterminer le nom
	instanceType := "primary"
	if !isPrimary {
		instanceType = "secondary"
	}
	name := fmt.Sprintf("%s-%s", mariadbHA.Name, instanceType)
	
	// Récupérer le StatefulSet
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: mariadbHA.Namespace}, sts)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil // Pas prêt car inexistant
		}
		return false, err
	}
	
	// Vérifier si le StatefulSet est prêt
	return sts.Status.ReadyReplicas == *sts.Spec.Replicas, nil
}

// Fonctions utilitaires

// mergeMySQLConfigs fusionne la configuration commune avec la configuration spécifique
func mergeMySQLConfigs(common, specific databasev1alpha1.MariaDBConfig) databasev1alpha1.MariaDBConfig {
	result := databasev1alpha1.MariaDBConfig{}
	
	// Version
	if specific.Version != "" {
		result.Version = specific.Version
	} else {
		result.Version = common.Version
	}
	
	// Image
	if specific.Image != "" {
		result.Image = specific.Image
	} else {
		result.Image = common.Image
	}
	
	// Paramètres de configuration - priorité à la config spécifique
	result.ConfigurationParameters = make(map[string]string)
	if common.ConfigurationParameters != nil {
		for k, v := range common.ConfigurationParameters {
			result.ConfigurationParameters[k] = v
		}
	}
	if specific.ConfigurationParameters != nil {
		for k, v := range specific.ConfigurationParameters {
			result.ConfigurationParameters[k] = v
		}
	}
	
	// Secret des credentials - priorité à la config spécifique
	if specific.UserCredentialsSecret != nil {
		result.UserCredentialsSecret = specific.UserCredentialsSecret
	} else {
		result.UserCredentialsSecret = common.UserCredentialsSecret
	}
	
	return result
}

// needsUpdate vérifie si un StatefulSet doit être mis à jour
func needsUpdate(current, desired *appsv1.StatefulSet) bool {
	// Vérifier les replicas
	if *current.Spec.Replicas != *desired.Spec.Replicas {
		return true
	}
	
	// Vérifier l'image du conteneur
	if len(current.Spec.Template.Spec.Containers) > 0 && len(desired.Spec.Template.Spec.Containers) > 0 {
		if current.Spec.Template.Spec.Containers[0].Image != desired.Spec.Template.Spec.Containers[0].Image {
			return true
		}
	}
	
	// Vérifier les ressources
	if len(current.Spec.Template.Spec.Containers) > 0 && len(desired.Spec.Template.Spec.Containers) > 0 {
		if !reflect.DeepEqual(current.Spec.Template.Spec.Containers[0].Resources, desired.Spec.Template.Spec.Containers[0].Resources) {
			return true
		}
	}
	
	// Vérifier les volumes
	if !reflect.DeepEqual(current.Spec.Template.Spec.Volumes, desired.Spec.Template.Spec.Volumes) {
		return true
	}
	
	return false
}

// getAccessModes retourne les modes d'accès pour le PVC, avec une valeur par défaut
func getAccessModes(modes []corev1.PersistentVolumeAccessMode) []corev1.PersistentVolumeAccessMode {
	if len(modes) == 0 {
		return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	return modes
}

// int32Ptr retourne un pointeur vers une valeur int32
func int32Ptr(i int32) *int32 {
	return &i
}

// boolPtr retourne un pointeur vers une valeur bool
func boolPtr(b bool) *bool {
	return &b
}