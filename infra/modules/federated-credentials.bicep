targetScope = 'resourceGroup'

param oidcIssuerUrl string
param apiIdentityName string
param brokerIdentityName string
param bootstrapIdentityName string

var systemNamespace = 'devsandbox-system'
var azureAdTokenExchangeAudience = 'api://AzureADTokenExchange'

resource apiIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30' existing = {
  name: apiIdentityName
}

resource brokerIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30' existing = {
  name: brokerIdentityName
}

resource bootstrapIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30' existing = {
  name: bootstrapIdentityName
}

resource apiFederatedCredential 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2024-11-30' = {
  parent: apiIdentity
  name: 'devsandbox-api'
  properties: {
    audiences: [
      azureAdTokenExchangeAudience
    ]
    issuer: oidcIssuerUrl
    subject: 'system:serviceaccount:${systemNamespace}:devsandbox-api'
  }
}

resource brokerFederatedCredential 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2024-11-30' = {
  parent: brokerIdentity
  name: 'devsandbox-broker'
  properties: {
    audiences: [
      azureAdTokenExchangeAudience
    ]
    issuer: oidcIssuerUrl
    subject: 'system:serviceaccount:${systemNamespace}:devsandbox-broker'
  }
}

resource bootstrapFederatedCredential 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2024-11-30' = {
  parent: bootstrapIdentity
  name: 'devsandbox-bootstrap'
  properties: {
    audiences: [
      azureAdTokenExchangeAudience
    ]
    issuer: oidcIssuerUrl
    subject: 'system:serviceaccount:${systemNamespace}:devsandbox-bootstrap'
  }
}

output apiFederatedCredentialResourceId string = apiFederatedCredential.id
output brokerFederatedCredentialResourceId string = brokerFederatedCredential.id
output bootstrapFederatedCredentialResourceId string = bootstrapFederatedCredential.id
