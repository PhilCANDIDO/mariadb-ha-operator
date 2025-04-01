# Exemple de configuration MariaDBHA

Ce document explique comment configurer une instance MariaDBHA pour une haute disponibilité dans Kubernetes.

## Exemple complet

- [database_v1alpha1_mariadbha.yaml](../../config/samples/database_v1alpha1_mariadbha.yaml)

## Explication des sections clés

### Instances

Cette section définit la configuration des serveurs MariaDB primaire et secondaire :

- **commonConfig** : Configuration partagée entre toutes les instances
- **primary** : Configuration spécifique au serveur primaire
- **secondary** : Configuration spécifique au serveur secondaire

### Replication

Définit comment la réplication est configurée entre le primaire et le secondaire :

- **mode** : Mode de réplication (async ou semisync)
- **automaticFailover** : Active le basculement automatique
- **maxLagSeconds** : Retard maximum acceptable pour le secondaire

### Failover

Contrôle le comportement lors d'un basculement :

- **failureDetection** : Comment détecter une défaillance
- **promotionStrategy** : Stratégie de promotion (safe ou immediate)
- **readOnlyMode** : Si activé, le nouveau primaire sera promu en lecture seule

## Exemples d'utilisation

### Environnement de production

Utilisez des ressources plus importantes et une stratégie de failover sécurisée :

```yaml
spec:
  instances:
    primary:
      resources:
        requests:
          cpu: "2"
          memory: "8Gi"
  failover:
    promotionStrategy: "safe"
```

### Environnement de développement

Configuration plus légère et basculement plus rapide :

```yaml
spec:
  instances:
    primary:
      resources:
        requests:
          cpu: "0.5"
          memory: "1Gi"
  failover:
    promotionStrategy: "immediate"
```
EOF

# Ajouter une entrée dans le README pour mentionner ces exemples
echo "
## Exemples

Des exemples de configurations sont disponibles dans :
- \`config/samples/\` - Manifestes YAML prêts à l'emploi
- \`docs/examples/\` - Documentation détaillée avec des explications