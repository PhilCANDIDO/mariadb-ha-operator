// api/v1alpha1/mariadbha_types.go
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MariaDBHASpec définit les spécifications souhaitées pour la ressource MariaDBHA
type MariaDBHASpec struct {
	// Instances définit la configuration des instances MariaDB primaire et secondaire
	Instances InstancesConfig `json:"instances"`

	// Replication définit les paramètres de réplication entre le primaire et le secondaire
	Replication ReplicationConfig `json:"replication"`

	// Failover définit les paramètres de détection et de gestion du failover
	Failover FailoverConfig `json:"failover"`

	// Monitoring définit les options de surveillance
	// +optional
	Monitoring MonitoringConfig `json:"monitoring,omitempty"`

	// Backup définit les paramètres de sauvegarde
	// +optional
	Backup BackupConfig `json:"backup,omitempty"`
}

// InstancesConfig définit la configuration des instances MariaDB
type InstancesConfig struct {
	// Primary définit la configuration de l'instance primaire
	Primary InstanceConfig `json:"primary"`

	// Secondary définit la configuration de l'instance secondaire
	Secondary InstanceConfig `json:"secondary"`

	// CommonConfig définit les configurations communes aux deux instances
	// +optional
	CommonConfig MariaDBConfig `json:"commonConfig,omitempty"`
}

// InstanceConfig définit la configuration d'une instance MariaDB
type InstanceConfig struct {
	// Resources définit les ressources CPU et mémoire pour l'instance
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Storage définit les paramètres de stockage
	Storage StorageConfig `json:"storage"`

	// NodeSelector permet de cibler des nœuds spécifiques
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations permet de définir des tolérances pour le scheduling
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Config définit les paramètres spécifiques de cette instance MariaDB
	// Ils seront fusionnés avec CommonConfig, cette config ayant la priorité
	// +optional
	Config MariaDBConfig `json:"config,omitempty"`

	// PodAnnotations permet d'ajouter des annotations au pod
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
}

// StorageConfig définit les options de stockage pour une instance
type StorageConfig struct {
	// StorageClassName est le nom de la classe de stockage à utiliser
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// Size définit la taille du volume persistant
	Size string `json:"size"`

	// AccessModes définit les modes d'accès du PVC
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
}

// MariaDBConfig définit la configuration spécifique de MariaDB
type MariaDBConfig struct {
	// Version définit la version de MariaDB à utiliser
	// +optional
	Version string `json:"version,omitempty"`

	// Image définit l'image Docker à utiliser
	// +optional
	Image string `json:"image,omitempty"`

	// ConfigurationParameters contient les paramètres de configuration MariaDB
	// à ajouter dans my.cnf
	// +optional
	ConfigurationParameters map[string]string `json:"configurationParameters,omitempty"`

	// UserCredentialsSecret référence le secret contenant les identifiants pour se connecter à MariaDB
	// +optional
	UserCredentialsSecret *SecretReference `json:"userCredentialsSecret,omitempty"`
}

// SecretReference contient la référence à un secret Kubernetes
type SecretReference struct {
	// Name est le nom du secret
	Name string `json:"name"`

	// Keys est un mapping des clés du secret vers leur utilisation
	// +optional
	Keys map[string]string `json:"keys,omitempty"`
}

// ReplicationConfig définit les paramètres de réplication
type ReplicationConfig struct {
	// Mode définit le mode de réplication (async par défaut)
	// +kubebuilder:validation:Enum=async;semisync
	// +optional
	Mode string `json:"mode,omitempty"`

	// User définit l'utilisateur de réplication
	// +optional
	User string `json:"user,omitempty"`

	// AutomaticFailover indique si la promotion du secondaire doit être automatique
	// +optional
	AutomaticFailover *bool `json:"automaticFailover,omitempty"`

	// MaxLagSeconds définit le retard maximum acceptable pour le secondaire
	// +optional
	MaxLagSeconds *int32 `json:"maxLagSeconds,omitempty"`

	// ReconnectRetries définit le nombre de tentatives de reconnexion de la réplication
	// +optional
	ReconnectRetries *int32 `json:"reconnectRetries,omitempty"`
}

// FailoverConfig définit la configuration du failover
type FailoverConfig struct {
	// FailureDetection définit les paramètres de détection de défaillance
	FailureDetection FailureDetectionConfig `json:"failureDetection"`

	// PromotionStrategy définit la stratégie de promotion du secondaire
	// +kubebuilder:validation:Enum=safe;immediate
	// +optional
	PromotionStrategy string `json:"promotionStrategy,omitempty"`

	// MinimumIntervalSeconds définit l'intervalle minimum entre deux failovers
	// +optional
	MinimumIntervalSeconds *int32 `json:"minimumIntervalSeconds,omitempty"`

	// AutomaticFailback indique si l'ancien primaire doit être réintégré comme
	// secondaire après sa récupération
	// +optional
	AutomaticFailback *bool `json:"automaticFailback,omitempty"`

	// ReadOnlyMode indique si le nouveau primaire doit être promu en lecture seule
	// après le failover (nécessitant une intervention manuelle pour activer l'écriture)
	// +optional
	ReadOnlyMode *bool `json:"readOnlyMode,omitempty"`
}

// FailureDetectionConfig définit les paramètres de détection de défaillance
type FailureDetectionConfig struct {
	// HealthCheckIntervalSeconds définit l'intervalle entre les vérifications de santé
	// +optional
	HealthCheckIntervalSeconds *int32 `json:"healthCheckIntervalSeconds,omitempty"`

	// FailureThresholdCount définit le nombre de vérifications échouées consécutives
	// avant de considérer une instance comme défaillante
	// +optional
	FailureThresholdCount *int32 `json:"failureThresholdCount,omitempty"`

	// TimeoutSeconds définit le délai d'attente pour les vérifications de santé
	// +optional
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// HealthChecks définit les types de vérifications à effectuer
	// +optional
	HealthChecks []string `json:"healthChecks,omitempty"`
}

// MonitoringConfig définit les options de surveillance
type MonitoringConfig struct {
	// Enabled indique si la surveillance est activée
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// PrometheusExporter indique si l'exporteur Prometheus doit être déployé
	// +optional
	PrometheusExporter *bool `json:"prometheusExporter,omitempty"`

	// ServiceMonitor indique si un ServiceMonitor doit être créé
	// pour l'intégration avec Prometheus Operator
	// +optional
	ServiceMonitor *bool `json:"serviceMonitor,omitempty"`

	// ExporterImage définit l'image de l'exporteur à utiliser
	// +optional
	ExporterImage string `json:"exporterImage,omitempty"`
}

// BackupConfig définit les options de sauvegarde
type BackupConfig struct {
	// Enabled indique si les sauvegardes sont activées
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Schedule définit la planification cron des sauvegardes
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// RetentionPolicy définit la politique de rétention des sauvegardes
	// +optional
	RetentionPolicy string `json:"retentionPolicy,omitempty"`

	// StorageLocation définit l'emplacement de stockage des sauvegardes
	// +optional
	StorageLocation string `json:"storageLocation,omitempty"`
}

// MariaDBHAStatus définit l'état observé de la ressource MariaDBHA
type MariaDBHAStatus struct {
	// Phase indique la phase actuelle du cluster (Initializing, Running, Failing, Failover, etc.)
	Phase string `json:"phase"`

	// CurrentPrimary indique le nom du pod qui est actuellement primaire
	CurrentPrimary string `json:"currentPrimary"`

	// CurrentSecondary indique le nom du pod qui est actuellement secondaire
	CurrentSecondary string `json:"currentSecondary"`

	// LastFailoverTime indique la date du dernier failover
	// +optional
	LastFailoverTime *metav1.Time `json:"lastFailoverTime,omitempty"`

	// ReplicationLag indique le retard de réplication en secondes
	// +optional
	ReplicationLag *int32 `json:"replicationLag,omitempty"`

	// Conditions représente les conditions du système
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// FailoverHistory contient l'historique des failovers
	// +optional
	FailoverHistory []FailoverEvent `json:"failoverHistory,omitempty"`
}

// FailoverEvent représente un événement de failover
type FailoverEvent struct {
	// Timestamp indique la date du failover
	Timestamp metav1.Time `json:"timestamp"`

	// Reason indique la raison du failover
	Reason string `json:"reason"`

	// OldPrimary indique l'ancien primaire
	OldPrimary string `json:"oldPrimary"`

	// NewPrimary indique le nouveau primaire
	NewPrimary string `json:"newPrimary"`

	// Automatic indique si le failover était automatique
	Automatic bool `json:"automatic"`

	// Duration indique la durée du failover en secondes
	// +optional
	Duration *int32 `json:"duration,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Primary",type="string",JSONPath=".status.currentPrimary"
//+kubebuilder:printcolumn:name="Secondary",type="string",JSONPath=".status.currentSecondary"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MariaDBHA est la définition de l'API pour le contrôleur MariaDBHA
type MariaDBHA struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MariaDBHASpec   `json:"spec,omitempty"`
	Status MariaDBHAStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MariaDBHAList contient une liste de ressources MariaDBHA
type MariaDBHAList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MariaDBHA `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MariaDBHA{}, &MariaDBHAList{})
}