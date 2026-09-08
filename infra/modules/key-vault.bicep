targetScope = 'resourceGroup'

param location string
param vaultName string
param apiPrincipalId string
param brokerPrincipalId string
param bootstrapPrincipalId string
param privateEndpointSubnetResourceId string
param virtualNetworkResourceId string
param tags object

var apiRoleDefinitionGuid = guid(subscription().id, resourceGroup().id, 'DevSandbox API Key Vault Data')
var brokerRoleDefinitionGuid = guid(subscription().id, resourceGroup().id, 'DevSandbox Broker Key Vault Data')
var keyVaultAdministratorRoleDefinitionId = subscriptionResourceId(
  'Microsoft.Authorization/roleDefinitions',
  '00482a5a-887f-4fb3-b363-3b7fe8e74483'
)

resource vault 'Microsoft.KeyVault/vaults@2024-11-01' = {
  name: vaultName
  location: location
  tags: tags
  properties: {
    enablePurgeProtection: true
    enableRbacAuthorization: true
    enableSoftDelete: true
    publicNetworkAccess: 'Disabled'
    sku: {
      family: 'A'
      name: 'standard'
    }
    softDeleteRetentionInDays: 90
    tenantId: tenant().tenantId
  }
}

resource privateDnsZone 'Microsoft.Network/privateDnsZones@2024-06-01' = {
  name: 'privatelink.vaultcore.azure.net'
  location: 'global'
  tags: tags
}

resource privateDnsLink 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2024-06-01' = {
  parent: privateDnsZone
  name: 'devsandbox-vnet'
  location: 'global'
  properties: {
    registrationEnabled: false
    virtualNetwork: {
      id: virtualNetworkResourceId
    }
  }
}

resource privateEndpoint 'Microsoft.Network/privateEndpoints@2025-01-01' = {
  name: '${vaultName}-pe'
  location: location
  tags: tags
  properties: {
    privateLinkServiceConnections: [
      {
        name: 'key-vault'
        properties: {
          groupIds: [
            'vault'
          ]
          privateLinkServiceId: vault.id
        }
      }
    ]
    subnet: {
      id: privateEndpointSubnetResourceId
    }
  }
}

resource privateDnsZoneGroup 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups@2025-01-01' = {
  parent: privateEndpoint
  name: 'default'
  properties: {
    privateDnsZoneConfigs: [
      {
        name: 'key-vault'
        properties: {
          privateDnsZoneId: privateDnsZone.id
        }
      }
    ]
  }
}

resource routeSigningKey 'Microsoft.KeyVault/vaults/keys@2024-11-01' = {
  parent: vault
  name: 'devsandbox-route-signing'
  properties: {
    attributes: {
      enabled: true
    }
    keyOps: [
      'sign'
      'verify'
    ]
    keySize: 3072
    kty: 'RSA'
  }
}

resource apiKeyVaultRole 'Microsoft.Authorization/roleDefinitions@2022-04-01' = {
  name: apiRoleDefinitionGuid
  properties: {
    assignableScopes: [
      resourceGroup().id
    ]
    description: 'Minimum Key Vault secret rotation and route-signing key actions for the DevSandbox API.'
    permissions: [
      {
        actions: []
        dataActions: [
          'Microsoft.KeyVault/vaults/secrets/getSecret/action'
          'Microsoft.KeyVault/vaults/secrets/readMetadata/action'
          'Microsoft.KeyVault/vaults/secrets/setSecret/action'
          'Microsoft.KeyVault/vaults/keys/read'
          'Microsoft.KeyVault/vaults/keys/sign/action'
          'Microsoft.KeyVault/vaults/keys/verify/action'
        ]
        notActions: []
        notDataActions: []
      }
    ]
    roleName: 'DevSandbox API Key Vault Data'
    type: 'CustomRole'
  }
}

resource brokerKeyVaultRole 'Microsoft.Authorization/roleDefinitions@2022-04-01' = {
  name: brokerRoleDefinitionGuid
  properties: {
    assignableScopes: [
      resourceGroup().id
    ]
    description: 'Minimum Key Vault secret read and rotation actions for the DevSandbox credential broker.'
    permissions: [
      {
        actions: []
        dataActions: [
          'Microsoft.KeyVault/vaults/secrets/getSecret/action'
          'Microsoft.KeyVault/vaults/secrets/readMetadata/action'
          'Microsoft.KeyVault/vaults/secrets/setSecret/action'
        ]
        notActions: []
        notDataActions: []
      }
    ]
    roleName: 'DevSandbox Broker Key Vault Data'
    type: 'CustomRole'
  }
}

resource apiVaultAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(vault.id, apiPrincipalId, apiRoleDefinitionGuid)
  scope: vault
  properties: {
    principalId: apiPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: apiKeyVaultRole.id
  }
}

resource brokerVaultAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(vault.id, brokerPrincipalId, brokerRoleDefinitionGuid)
  scope: vault
  properties: {
    principalId: brokerPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: brokerKeyVaultRole.id
  }
}

resource bootstrapVaultAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(vault.id, bootstrapPrincipalId, keyVaultAdministratorRoleDefinitionId)
  scope: vault
  properties: {
    principalId: bootstrapPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: keyVaultAdministratorRoleDefinitionId
  }
}

output vaultName string = vault.name
output vaultResourceId string = vault.id
output vaultUri string = vault.properties.vaultUri
output privateEndpointResourceId string = privateEndpoint.id
