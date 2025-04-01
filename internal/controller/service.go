// internal/controller/service.go
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/PhilCANDIDO/mariadb-ha-operator/api/v1alpha1"
)

// reconcileServices crée ou met à jour les Services pour les instances MariaDB
func (r *MariaDBHAReconciler) reconcileServices(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Réconciliation des Services")

	// Réconcilier le service headless pour le StatefulSet du primaire
	result, err := r.reconcileHeadlessService(ctx, mariadbHA, true)
	if err != nil || result.Requeue {
		return result, err
	}

	// Réconcilier le service headless pour le StatefulSet du secondaire
	result, err = r.reconcileHeadlessService(ctx, mariadbHA, false)
	if err != nil || result.Requeue {
		return result, err
	}

	// Réconcilier le service principal (pour les clients)
	result, err = r.reconcileClusterService(ctx, mariadbHA)
	if err != nil || result.Requeue {
		return result, err
	}

	// Réconcilier le service pour le read-only (optionnel)
	if r.shouldCreateReadOnlyService(mariadbHA) {
		result, err = r.reconcileReadOnlyService(ctx, mariadbHA)
		if err != nil || result.Requeue {
			return result, err
		}
	}

	return ctrl.Result{}, nil
}

// reconcileHeadlessService crée ou met à jour le service headless pour un StatefulSet
func (r *MariaDBHAReconciler) reconcileHeadlessService(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA, isPrimary bool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Déterminer le nom et les labels
	instanceType := "primary"
	if !isPrimary {
		instanceType = "secondary"
	}
	name := fmt.Sprintf("%s-%s", mariadbHA.Name, instanceType)

	labels := map[string]string{
		"app": "mariadb",
		"mariadb.cpf-it.fr/cluster": mariadbHA.Name,
		"mariadb.cpf-it.fr/role":    instanceType,
	}

	// Créer le service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mariadbHA.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector:  labels,
			ClusterIP: "None", // Service headless
			Ports: []corev1.ServicePort{
				{
					Name:       "mysql",
					Port:       3306,
					TargetPort: intstr.FromInt(3306),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	// Définir la référence pour que le service soit supprimé avec le MariaDBHA
	if err := controllerutil.SetControllerReference(mariadbHA, svc, r.Scheme); err != nil {
		logger.Error(err, "Impossible de définir la référence du contrôleur pour le service headless")
		return ctrl.Result{}, err
	}

	// Récupérer le service existant s'il existe
	found := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: mariadbHA.Namespace}, found)

	// Créer un nouveau service s'il n'existe pas
	if err != nil && errors.IsNotFound(err) {
		logger.Info("Création du service headless", "name", name)
		if err := r.Create(ctx, svc); err != nil {
			logger.Error(err, "Impossible de créer le service headless")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		logger.Error(err, "Erreur lors de la récupération du service headless")
		return ctrl.Result{}, err
	}

	// Service existe déjà, pas besoin de le mettre à jour car il est headless
	return ctrl.Result{}, nil
}

// reconcileClusterService crée ou met à jour le service principal pour les clients
func (r *MariaDBHAReconciler) reconcileClusterService(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Nom et labels du service
	name := fmt.Sprintf("%s-mariadb", mariadbHA.Name)

	// Sélecteur qui pointe vers le primaire
	selector := map[string]string{
		"app": "mariadb",
		"mariadb.cpf-it.fr/cluster": mariadbHA.Name,
		"mariadb.cpf-it.fr/role":    "primary",
	}

	// Créer le service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mariadbHA.Namespace,
			Labels: map[string]string{
				"app": "mariadb",
				"mariadb.cpf-it.fr/cluster": mariadbHA.Name,
				"mariadb.cpf-it.fr/service": "read-write",
			},
			Annotations: map[string]string{
				"mariadb.cpf-it.fr/service-type": "cluster",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Type:     corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "mysql",
					Port:       3306,
					TargetPort: intstr.FromInt(3306),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	// Définir la référence pour que le service soit supprimé avec le MariaDBHA
	if err := controllerutil.SetControllerReference(mariadbHA, svc, r.Scheme); err != nil {
		logger.Error(err, "Impossible de définir la référence du contrôleur pour le service cluster")
		return ctrl.Result{}, err
	}

	// Récupérer le service existant s'il existe
	found := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: mariadbHA.Namespace}, found)

	// Créer un nouveau service s'il n'existe pas
	if err != nil && errors.IsNotFound(err) {
		logger.Info("Création du service cluster", "name", name)
		if err := r.Create(ctx, svc); err != nil {
			logger.Error(err, "Impossible de créer le service cluster")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		logger.Error(err, "Erreur lors de la récupération du service cluster")
		return ctrl.Result{}, err
	}

	// Vérifier si le service doit être mis à jour (sélecteurs notamment)
	if !mapsEqual(found.Spec.Selector, selector) {
		found.Spec.Selector = selector
		logger.Info("Mise à jour du service cluster", "name", name)
		if err := r.Update(ctx, found); err != nil {
			logger.Error(err, "Impossible de mettre à jour le service cluster")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

// reconcileReadOnlyService crée ou met à jour le service pour les requêtes en lecture seule
func (r *MariaDBHAReconciler) reconcileReadOnlyService(ctx context.Context, mariadbHA *databasev1alpha1.MariaDBHA) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Nom et labels du service
	name := fmt.Sprintf("%s-mariadb-ro", mariadbHA.Name)

	// Sélecteur qui pointe vers le secondaire
	selector := map[string]string{
		"app": "mariadb",
		"mariadb.cpf-it.fr/cluster": mariadbHA.Name,
		"mariadb.cpf-it.fr/role":    "secondary",
	}

	// Créer le service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mariadbHA.Namespace,
			Labels: map[string]string{
				"app": "mariadb",
				"mariadb.cpf-it.fr/cluster": mariadbHA.Name,
				"mariadb.cpf-it.fr/service": "read-only",
			},
			Annotations: map[string]string{
				"mariadb.cpf-it.fr/service-type": "read-only",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Type:     corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "mysql",
					Port:       3306,
					TargetPort: intstr.FromInt(3306),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	// Définir la référence pour que le service soit supprimé avec le MariaDBHA
	if err := controllerutil.SetControllerReference(mariadbHA, svc, r.Scheme); err != nil {
		logger.Error(err, "Impossible de définir la référence du contrôleur pour le service read-only")
		return ctrl.Result{}, err
	}

	// Récupérer le service existant s'il existe
	found := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: mariadbHA.Namespace}, found)

	// Créer un nouveau service s'il n'existe pas
	if err != nil && errors.IsNotFound(err) {
		logger.Info("Création du service read-only", "name", name)
		if err := r.Create(ctx, svc); err != nil {
			logger.Error(err, "Impossible de créer le service read-only")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		logger.Error(err, "Erreur lors de la récupération du service read-only")
		return ctrl.Result{}, err
	}

	// Vérifier si le service doit être mis à jour (sélecteurs notamment)
	if !mapsEqual(found.Spec.Selector, selector) {
		found.Spec.Selector = selector
		logger.Info("Mise à jour du service read-only", "name", name)
		if err := r.Update(ctx, found); err != nil {
			logger.Error(err, "Impossible de mettre à jour le service read-only")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

// shouldCreateReadOnlyService détermine si un service read-only doit être créé
func (r *MariaDBHAReconciler) shouldCreateReadOnlyService(mariadbHA *databasev1alpha1.MariaDBHA) bool {
	// Par défaut, créer un service read-only
	// Cette fonction pourrait être étendue pour examiner les spécifications du CR
	return true
}

// mapsEqual vérifie si deux maps sont égales
func mapsEqual(map1, map2 map[string]string) bool {
	if len(map1) != len(map2) {
		return false
	}
	for k, v1 := range map1 {
		if v2, ok := map2[k]; !ok || v1 != v2 {
			return false
		}
	}
	return true
}