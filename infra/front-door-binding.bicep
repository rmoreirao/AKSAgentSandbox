targetScope = 'resourceGroup'

@description('Existing Azure Front Door Standard profile name.')
param profileName string

@description('Existing API Front Door endpoint name.')
param apiEndpointName string

@description('Existing web Front Door endpoint name.')
param webEndpointName string

@description('Public address reported by the AKS Gateway.')
param gatewayAddress string

@description('Internal, non-public API Host header expected by the AKS Gateway.')
param apiOriginHostname string

@description('Internal, non-public web Host header expected by the AKS Gateway.')
param webOriginHostname string

resource profile 'Microsoft.Cdn/profiles@2025-06-01' existing = {
  name: profileName
}

resource apiEndpoint 'Microsoft.Cdn/profiles/afdEndpoints@2025-06-01' existing = {
  parent: profile
  name: apiEndpointName
}

resource webEndpoint 'Microsoft.Cdn/profiles/afdEndpoints@2025-06-01' existing = {
  parent: profile
  name: webEndpointName
}

resource apiOriginGroup 'Microsoft.Cdn/profiles/originGroups@2025-06-01' = {
  parent: profile
  name: 'devsandbox-api'
  properties: {
    healthProbeSettings: {
      probeIntervalInSeconds: 30
      probePath: '/readyz'
      probeProtocol: 'Http'
      probeRequestType: 'GET'
    }
    loadBalancingSettings: {
      additionalLatencyInMilliseconds: 0
      sampleSize: 4
      successfulSamplesRequired: 3
    }
    sessionAffinityState: 'Disabled'
  }
}

resource webOriginGroup 'Microsoft.Cdn/profiles/originGroups@2025-06-01' = {
  parent: profile
  name: 'devsandbox-web'
  properties: {
    healthProbeSettings: {
      probeIntervalInSeconds: 30
      probePath: '/healthz'
      probeProtocol: 'Http'
      probeRequestType: 'GET'
    }
    loadBalancingSettings: {
      additionalLatencyInMilliseconds: 0
      sampleSize: 4
      successfulSamplesRequired: 3
    }
    sessionAffinityState: 'Disabled'
  }
}

resource apiOrigin 'Microsoft.Cdn/profiles/originGroups/origins@2025-06-01' = {
  parent: apiOriginGroup
  name: 'aks-gateway'
  properties: {
    enabledState: 'Enabled'
    enforceCertificateNameCheck: false
    hostName: gatewayAddress
    httpPort: 80
    httpsPort: 443
    originHostHeader: apiOriginHostname
    priority: 1
    weight: 1000
  }
}

resource webOrigin 'Microsoft.Cdn/profiles/originGroups/origins@2025-06-01' = {
  parent: webOriginGroup
  name: 'aks-gateway'
  properties: {
    enabledState: 'Enabled'
    enforceCertificateNameCheck: false
    hostName: gatewayAddress
    httpPort: 80
    httpsPort: 443
    originHostHeader: webOriginHostname
    priority: 1
    weight: 1000
  }
}

resource apiRoute 'Microsoft.Cdn/profiles/afdEndpoints/routes@2025-06-01' = {
  parent: apiEndpoint
  name: 'api'
  properties: {
    enabledState: 'Enabled'
    forwardingProtocol: 'HttpOnly'
    httpsRedirect: 'Disabled'
    linkToDefaultDomain: 'Enabled'
    originGroup: {
      id: apiOriginGroup.id
    }
    patternsToMatch: [
      '/*'
    ]
    supportedProtocols: [
      'Https'
    ]
  }
  dependsOn: [
    apiOrigin
  ]
}

resource webRoute 'Microsoft.Cdn/profiles/afdEndpoints/routes@2025-06-01' = {
  parent: webEndpoint
  name: 'web'
  properties: {
    enabledState: 'Enabled'
    forwardingProtocol: 'HttpOnly'
    httpsRedirect: 'Disabled'
    linkToDefaultDomain: 'Enabled'
    originGroup: {
      id: webOriginGroup.id
    }
    patternsToMatch: [
      '/*'
    ]
    supportedProtocols: [
      'Https'
    ]
  }
  dependsOn: [
    webOrigin
  ]
}

output apiRouteResourceId string = apiRoute.id
output webRouteResourceId string = webRoute.id
