targetScope = 'resourceGroup'

param profileName string
param apiEndpointName string
param webEndpointName string
param logAnalyticsWorkspaceResourceId string
param tags object

resource profile 'Microsoft.Cdn/profiles@2025-06-01' = {
  name: profileName
  location: 'global'
  tags: tags
  sku: {
    name: 'Standard_AzureFrontDoor'
  }
  properties: {
    originResponseTimeoutSeconds: 60
  }
}

resource apiEndpoint 'Microsoft.Cdn/profiles/afdEndpoints@2025-06-01' = {
  parent: profile
  name: apiEndpointName
  location: 'global'
  tags: tags
  properties: {
    enabledState: 'Enabled'
  }
}

resource webEndpoint 'Microsoft.Cdn/profiles/afdEndpoints@2025-06-01' = {
  parent: profile
  name: webEndpointName
  location: 'global'
  tags: tags
  properties: {
    enabledState: 'Enabled'
  }
}

#disable-next-line use-recent-api-versions
resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  name: 'devsandbox-front-door'
  scope: profile
  properties: {
    logAnalyticsDestinationType: 'Dedicated'
    workspaceId: logAnalyticsWorkspaceResourceId
    logs: [
      {
        category: 'FrontDoorAccessLog'
        enabled: true
      }
      {
        category: 'FrontDoorHealthProbeLog'
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

output profileName string = profile.name
output profileResourceId string = profile.id
output profileId string = profile.properties.frontDoorId
output apiEndpointName string = apiEndpoint.name
output apiEndpointHostname string = apiEndpoint.properties.hostName
output webEndpointName string = webEndpoint.name
output webEndpointHostname string = webEndpoint.properties.hostName
