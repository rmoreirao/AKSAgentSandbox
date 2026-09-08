targetScope = 'resourceGroup'

param location string
param registryName string
param deploymentPrincipalObjectId string = ''
param tags object

var acrPushRoleDefinitionId = subscriptionResourceId(
  'Microsoft.Authorization/roleDefinitions',
  '8311e382-0749-4cb8-b61a-304f252e45ec'
)

resource registry 'Microsoft.ContainerRegistry/registries@2025-11-01' = {
  name: registryName
  location: location
  tags: tags
  sku: {
    name: 'Standard'
  }
  properties: {
    adminUserEnabled: false
    anonymousPullEnabled: false
    dataEndpointEnabled: false
    publicNetworkAccess: 'Enabled'
  }
}

resource deploymentAcrPush 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(deploymentPrincipalObjectId)) {
  name: guid(registry.id, deploymentPrincipalObjectId, acrPushRoleDefinitionId)
  scope: registry
  properties: {
    principalId: deploymentPrincipalObjectId
    roleDefinitionId: acrPushRoleDefinitionId
  }
}

output registryName string = registry.name
output registryResourceId string = registry.id
output loginServer string = registry.properties.loginServer
