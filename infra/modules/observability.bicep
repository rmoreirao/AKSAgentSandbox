targetScope = 'resourceGroup'

param location string
param workspaceName string
param tags object

resource workspace 'Microsoft.OperationalInsights/workspaces@2025-02-01' = {
  name: workspaceName
  location: location
  tags: tags
  properties: {
    features: {
      enableLogAccessUsingOnlyResourcePermissions: true
    }
    publicNetworkAccessForIngestion: 'Enabled'
    publicNetworkAccessForQuery: 'Enabled'
    retentionInDays: 30
    sku: {
      name: 'PerGB2018'
    }
  }
}

output workspaceName string = workspace.name
output workspaceResourceId string = workspace.id
output workspaceCustomerId string = workspace.properties.customerId
