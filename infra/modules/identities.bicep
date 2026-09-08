targetScope = 'resourceGroup'

param location string
param aksIdentityName string
param apiIdentityName string
param brokerIdentityName string
param bootstrapIdentityName string
param tags object

resource aksIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30' = {
  name: aksIdentityName
  location: location
  tags: tags
}

resource apiIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30' = {
  name: apiIdentityName
  location: location
  tags: tags
}

resource brokerIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30' = {
  name: brokerIdentityName
  location: location
  tags: tags
}

resource bootstrapIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30' = {
  name: bootstrapIdentityName
  location: location
  tags: tags
}

output aksIdentityResourceId string = aksIdentity.id
output aksPrincipalId string = aksIdentity.properties.principalId
output apiIdentityResourceId string = apiIdentity.id
output apiPrincipalId string = apiIdentity.properties.principalId
output apiClientId string = apiIdentity.properties.clientId
output brokerIdentityResourceId string = brokerIdentity.id
output brokerPrincipalId string = brokerIdentity.properties.principalId
output brokerClientId string = brokerIdentity.properties.clientId
output bootstrapPrincipalId string = bootstrapIdentity.properties.principalId
output bootstrapClientId string = bootstrapIdentity.properties.clientId
