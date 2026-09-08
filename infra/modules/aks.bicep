targetScope = 'resourceGroup'

param location string
param clusterName string
param clusterIdentityResourceId string
param clusterIdentityPrincipalId string
param subnetResourceId string
param vnetName string
param aksSubnetName string
param logAnalyticsWorkspaceResourceId string
param containerRegistryResourceId string
param deploymentPrincipalObjectId string = ''
param systemNodeVmSize string = 'Standard_D4s_v6'
@minValue(1)
param systemNodeCount int = 2
param kataNodeVmSize string = 'Standard_D16s_v6'

@allowed([
  1
])
param kataMinCount int = 1

@minValue(1)
param kataMaxCount int = 10

param kubernetesVersion string = ''
param tags object

var networkContributorRoleDefinitionId = subscriptionResourceId(
  'Microsoft.Authorization/roleDefinitions',
  '4d97b98b-1d4f-4787-a291-c67834d212e7'
)
var acrPullRoleDefinitionId = subscriptionResourceId(
  'Microsoft.Authorization/roleDefinitions',
  '7f951dda-4ed3-4680-a7ca-43fe172d538d'
)
var monitoringMetricsPublisherRoleDefinitionId = subscriptionResourceId(
  'Microsoft.Authorization/roleDefinitions',
  '3913510d-42f4-4e42-8a64-420c390055eb'
)
var runCommandRoleDefinitionGuid = guid(subscription().id, resourceGroup().id, 'DevSandbox AKS Run Command')

resource vnet 'Microsoft.Network/virtualNetworks@2025-01-01' existing = {
  name: vnetName
}

resource aksSubnet 'Microsoft.Network/virtualNetworks/subnets@2025-01-01' existing = {
  parent: vnet
  name: aksSubnetName
}

resource registry 'Microsoft.ContainerRegistry/registries@2025-11-01' existing = {
  name: last(split(containerRegistryResourceId, '/'))
}

resource workspace 'Microsoft.OperationalInsights/workspaces@2025-02-01' existing = {
  name: last(split(logAnalyticsWorkspaceResourceId, '/'))
}

resource subnetNetworkContributor 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(subnetResourceId, clusterIdentityPrincipalId, networkContributorRoleDefinitionId)
  scope: aksSubnet
  properties: {
    principalId: clusterIdentityPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: networkContributorRoleDefinitionId
  }
}

resource cluster 'Microsoft.ContainerService/managedClusters@2026-03-01' = {
  name: clusterName
  location: location
  tags: tags
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${clusterIdentityResourceId}': {}
    }
  }
  properties: {
    ...(empty(kubernetesVersion) ? {} : {
      kubernetesVersion: kubernetesVersion
    })
    addonProfiles: {
      azureKeyvaultSecretsProvider: {
        enabled: true
        config: {
          enableSecretRotation: 'false'
        }
      }
      omsAgent: {
        enabled: true
        config: {
          logAnalyticsWorkspaceResourceID: logAnalyticsWorkspaceResourceId
          useAADAuth: 'true'
        }
      }
    }
    agentPoolProfiles: [
      {
        name: 'system'
        count: systemNodeCount
        enableAutoScaling: true
        maxCount: 5
        maxPods: 50
        minCount: 1
        mode: 'System'
        osDiskSizeGB: 128
        osDiskType: 'Managed'
        osSKU: 'AzureLinux'
        osType: 'Linux'
        type: 'VirtualMachineScaleSets'
        vmSize: systemNodeVmSize
        vnetSubnetID: subnetResourceId
      }
    ]
    apiServerAccessProfile: {
      enablePrivateCluster: true
      enablePrivateClusterPublicFQDN: false
      privateDNSZone: 'system'
    }
    autoUpgradeProfile: {
      nodeOSUpgradeChannel: 'NodeImage'
      upgradeChannel: 'stable'
    }
    azureMonitorProfile: {
      metrics: {
        enabled: true
        kubeStateMetrics: {
          metricAnnotationsAllowList: ''
          metricLabelsAllowlist: ''
        }
      }
    }
    dnsPrefix: take('${clusterName}-dns', 54)
    enableRBAC: true
    ingressProfile: {
      gatewayAPI: {
        installation: 'Standard'
      }
      webAppRouting: {
        enabled: true
        gatewayAPIImplementations: {
          appRoutingIstio: {
            mode: 'Enabled'
          }
        }
      }
    }
    networkProfile: {
      dnsServiceIP: '10.1.0.10'
      loadBalancerSku: 'standard'
      networkDataplane: 'cilium'
      networkPlugin: 'azure'
      networkPluginMode: 'overlay'
      networkPolicy: 'cilium'
      outboundType: 'loadBalancer'
      serviceCidr: '10.1.0.0/16'
    }
    oidcIssuerProfile: {
      enabled: true
    }
    securityProfile: {
      workloadIdentity: {
        enabled: true
      }
    }
    storageProfile: {
      blobCSIDriver: {
        enabled: false
      }
      diskCSIDriver: {
        enabled: true
      }
      fileCSIDriver: {
        enabled: true
      }
      snapshotController: {
        enabled: true
      }
    }
  }
  dependsOn: [
    subnetNetworkContributor
  ]
}

resource kataPool 'Microsoft.ContainerService/managedClusters/agentPools@2026-03-01' = {
  parent: cluster
  name: 'kata'
  properties: {
    ...(empty(kubernetesVersion) ? {} : {
      orchestratorVersion: kubernetesVersion
    })
    count: kataMinCount
    enableAutoScaling: true
    maxCount: kataMaxCount
    maxPods: 50
    minCount: kataMinCount
    mode: 'User'
    nodeLabels: {
      'devsandbox.github.com/runtime': 'kata'
    }
    nodeTaints: [
      'devsandbox.github.com/runtime=kata:NoSchedule'
    ]
    osDiskSizeGB: 128
    osDiskType: 'Managed'
    osSKU: 'AzureLinux'
    osType: 'Linux'
    scaleDownMode: 'Delete'
    type: 'VirtualMachineScaleSets'
    vmSize: kataNodeVmSize
    vnetSubnetID: subnetResourceId
    workloadRuntime: 'KataVmIsolation'
  }
}

resource kubeletAcrPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(registry.id, cluster.id, acrPullRoleDefinitionId)
  scope: registry
  properties: {
    principalId: cluster.properties.identityProfile.kubeletidentity.objectId
    principalType: 'ServicePrincipal'
    roleDefinitionId: acrPullRoleDefinitionId
  }
}

resource monitoringMetricsPublisher 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(workspace.id, clusterIdentityPrincipalId, monitoringMetricsPublisherRoleDefinitionId)
  scope: workspace
  properties: {
    principalId: clusterIdentityPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: monitoringMetricsPublisherRoleDefinitionId
  }
}

#disable-next-line use-recent-api-versions
resource clusterDiagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  name: 'devsandbox-monitoring'
  scope: cluster
  properties: {
    logAnalyticsDestinationType: 'Dedicated'
    workspaceId: logAnalyticsWorkspaceResourceId
    logs: [
      {
        categoryGroup: 'allLogs'
        enabled: true
      }
    ]
    metrics: [
      {
        category: 'AllMetrics'
        enabled: true
      }
    ]
  }
}

resource runCommandRole 'Microsoft.Authorization/roleDefinitions@2022-04-01' = if (!empty(deploymentPrincipalObjectId)) {
  name: runCommandRoleDefinitionGuid
  properties: {
    assignableScopes: [
      resourceGroup().id
    ]
    description: 'Invoke AKS Run Command and read its result for staged DevSandbox deployment.'
    permissions: [
      {
        actions: [
          'Microsoft.ContainerService/managedClusters/runcommand/action'
          'Microsoft.ContainerService/managedClusters/commandResults/read'
        ]
        dataActions: []
        notActions: []
        notDataActions: []
      }
    ]
    roleName: 'DevSandbox AKS Run Command'
    type: 'CustomRole'
  }
}

resource deploymentRunCommand 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(deploymentPrincipalObjectId)) {
  name: guid(cluster.id, deploymentPrincipalObjectId, runCommandRoleDefinitionGuid)
  scope: cluster
  properties: {
    principalId: deploymentPrincipalObjectId
    roleDefinitionId: runCommandRole.id
  }
}

output clusterName string = cluster.name
output clusterResourceId string = cluster.id
output oidcIssuerUrl string = cluster.properties.oidcIssuerProfile.issuerURL
output kataPoolName string = kataPool.name
