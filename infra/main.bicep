targetScope = 'resourceGroup'

@description('Short DNS-safe prefix used in deterministic resource names.')
@minLength(3)
@maxLength(12)
param resourcePrefix string

@description('Environment label included in names and tags.')
@maxLength(12)
param environmentName string = 'poc'

param location string = resourceGroup().location
param tags object = {}
param deploymentPrincipalObjectId string = ''
param vnetAddressPrefix string = '10.240.0.0/16'
param aksSubnetAddressPrefix string = '10.240.0.0/20'
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

var normalizedPrefix = toLower(resourcePrefix)
var normalizedEnvironmentName = toLower(environmentName)
var suffix = take(uniqueString(subscription().id, resourceGroup().id, normalizedPrefix, normalizedEnvironmentName), 6)
var baseName = '${normalizedPrefix}-${normalizedEnvironmentName}-${suffix}'
var commonTags = union({
  application: 'devsandbox'
  environment: normalizedEnvironmentName
  'managed-by': 'bicep'
}, tags)
var names = {
  vnet: '${baseName}-vnet'
  workspace: '${baseName}-law'
  registry: take('${replace(normalizedPrefix, '-', '')}${replace(normalizedEnvironmentName, '-', '')}${suffix}cr', 50)
  vault: take('${normalizedPrefix}-${normalizedEnvironmentName}-${suffix}-kv', 24)
  aks: '${baseName}-aks'
  aksIdentity: '${baseName}-aks-id'
  apiIdentity: '${baseName}-api-id'
  brokerIdentity: '${baseName}-broker-id'
  bootstrapIdentity: '${baseName}-bootstrap-id'
  frontDoorProfile: '${baseName}-afd'
  frontDoorApiEndpoint: '${baseName}-api'
  frontDoorWebEndpoint: '${baseName}-web'
}
var apiOriginHostname = 'api-origin.${baseName}.internal'
var webOriginHostname = 'web-origin.${baseName}.internal'

module network './modules/network.bicep' = {
  name: 'network'
  params: {
    location: location
    vnetName: names.vnet
    vnetAddressPrefix: vnetAddressPrefix
    aksSubnetAddressPrefix: aksSubnetAddressPrefix
    tags: commonTags
  }
}

module observability './modules/observability.bicep' = {
  name: 'observability'
  params: {
    location: location
    workspaceName: names.workspace
    tags: commonTags
  }
}

module identities './modules/identities.bicep' = {
  name: 'identities'
  params: {
    location: location
    aksIdentityName: names.aksIdentity
    apiIdentityName: names.apiIdentity
    brokerIdentityName: names.brokerIdentity
    bootstrapIdentityName: names.bootstrapIdentity
    tags: commonTags
  }
}

module keyVault './modules/key-vault.bicep' = {
  name: 'key-vault'
  params: {
    location: location
    vaultName: names.vault
    apiPrincipalId: identities.outputs.apiPrincipalId
    brokerPrincipalId: identities.outputs.brokerPrincipalId
    bootstrapPrincipalId: identities.outputs.bootstrapPrincipalId
    privateEndpointSubnetResourceId: network.outputs.aksSubnetResourceId
    virtualNetworkResourceId: network.outputs.vnetResourceId
    tags: commonTags
  }
}

module acr './modules/acr.bicep' = {
  name: 'acr'
  params: {
    location: location
    registryName: names.registry
    deploymentPrincipalObjectId: deploymentPrincipalObjectId
    tags: commonTags
  }
}

module frontDoor './modules/front-door.bicep' = {
  name: 'front-door'
  params: {
    profileName: names.frontDoorProfile
    apiEndpointName: names.frontDoorApiEndpoint
    webEndpointName: names.frontDoorWebEndpoint
    logAnalyticsWorkspaceResourceId: observability.outputs.workspaceResourceId
    tags: commonTags
  }
}

module aks './modules/aks.bicep' = {
  name: 'aks'
  params: {
    location: location
    clusterName: names.aks
    clusterIdentityResourceId: identities.outputs.aksIdentityResourceId
    clusterIdentityPrincipalId: identities.outputs.aksPrincipalId
    subnetResourceId: network.outputs.aksSubnetResourceId
    vnetName: names.vnet
    aksSubnetName: network.outputs.aksSubnetName
    logAnalyticsWorkspaceResourceId: observability.outputs.workspaceResourceId
    containerRegistryResourceId: acr.outputs.registryResourceId
    deploymentPrincipalObjectId: deploymentPrincipalObjectId
    systemNodeVmSize: systemNodeVmSize
    systemNodeCount: systemNodeCount
    kataNodeVmSize: kataNodeVmSize
    kataMinCount: kataMinCount
    kataMaxCount: kataMaxCount
    kubernetesVersion: kubernetesVersion
    tags: commonTags
  }
}

module federation './modules/federated-credentials.bicep' = {
  name: 'federated-credentials'
  params: {
    oidcIssuerUrl: aks.outputs.oidcIssuerUrl
    apiIdentityName: names.apiIdentity
    brokerIdentityName: names.brokerIdentity
    bootstrapIdentityName: names.bootstrapIdentity
  }
}

output aksClusterName string = aks.outputs.clusterName
output aksClusterResourceId string = aks.outputs.clusterResourceId
output containerRegistryName string = acr.outputs.registryName
output containerRegistryLoginServer string = acr.outputs.loginServer
output keyVaultName string = keyVault.outputs.vaultName
output keyVaultUri string = keyVault.outputs.vaultUri
output logAnalyticsWorkspaceId string = observability.outputs.workspaceResourceId
output oidcIssuerUrl string = aks.outputs.oidcIssuerUrl
output apiIdentityClientId string = identities.outputs.apiClientId
output brokerIdentityClientId string = identities.outputs.brokerClientId
output bootstrapIdentityClientId string = identities.outputs.bootstrapClientId
output frontDoorProfileName string = frontDoor.outputs.profileName
output frontDoorProfileResourceId string = frontDoor.outputs.profileResourceId
output frontDoorProfileId string = frontDoor.outputs.profileId
output frontDoorApiEndpointName string = frontDoor.outputs.apiEndpointName
output frontDoorApiHostname string = frontDoor.outputs.apiEndpointHostname
output frontDoorWebEndpointName string = frontDoor.outputs.webEndpointName
output frontDoorWebHostname string = frontDoor.outputs.webEndpointHostname
output apiOriginHostname string = apiOriginHostname
output webOriginHostname string = webOriginHostname
