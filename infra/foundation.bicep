targetScope = 'subscription'

@description('Short DNS-safe prefix used in deterministic resource names.')
@minLength(3)
@maxLength(12)
param resourcePrefix string

@description('Azure region for the resource group and regional resources.')
param location string = 'westeurope'

@description('Environment label included in names and tags.')
@maxLength(12)
param environmentName string = 'poc'

@description('Additional tags applied to every supported resource.')
param tags object = {}

@description('Object ID of the deployment identity that builds images and invokes AKS Run Command.')
param deploymentPrincipalObjectId string = ''

@description('Virtual network address space.')
param vnetAddressPrefix string = '10.240.0.0/16'

@description('AKS node subnet address prefix.')
param aksSubnetAddressPrefix string = '10.240.0.0/20'

@description('System node virtual machine SKU.')
param systemNodeVmSize string = 'Standard_D4s_v6'

@description('Initial system node count. Autoscaling can adjust it after deployment.')
@minValue(1)
param systemNodeCount int = 2

@description('Kata node virtual machine SKU. Re-run large-profile scheduling validation after changing it.')
param kataNodeVmSize string = 'Standard_D16s_v6'

@allowed([
  1
])
param kataMinCount int = 1

@minValue(1)
param kataMaxCount int = 10

@description('Optional stable AKS Kubernetes version. Empty selects the regional default.')
param kubernetesVersion string = ''

var normalizedPrefix = toLower(resourcePrefix)
var normalizedEnvironmentName = toLower(environmentName)
var nameSuffix = take(uniqueString(subscription().id, normalizedPrefix, location), 6)
var resourceGroupName = '${normalizedPrefix}-${normalizedEnvironmentName}-${nameSuffix}-rg'
var commonTags = union({
  application: 'devsandbox'
  environment: normalizedEnvironmentName
  'managed-by': 'bicep'
}, tags)

resource foundationResourceGroup 'Microsoft.Resources/resourceGroups@2024-11-01' = {
  name: resourceGroupName
  location: location
  tags: commonTags
}

module foundation './main.bicep' = {
  name: 'devsandbox-foundation'
  scope: foundationResourceGroup
  params: {
    resourcePrefix: normalizedPrefix
    environmentName: normalizedEnvironmentName
    location: location
    tags: commonTags
    deploymentPrincipalObjectId: deploymentPrincipalObjectId
    vnetAddressPrefix: vnetAddressPrefix
    aksSubnetAddressPrefix: aksSubnetAddressPrefix
    systemNodeVmSize: systemNodeVmSize
    systemNodeCount: systemNodeCount
    kataNodeVmSize: kataNodeVmSize
    kataMinCount: kataMinCount
    kataMaxCount: kataMaxCount
    kubernetesVersion: kubernetesVersion
  }
}

output resourceGroupName string = foundationResourceGroup.name
output resourceGroupId string = foundationResourceGroup.id
output aksClusterName string = foundation.outputs.aksClusterName
output aksClusterResourceId string = foundation.outputs.aksClusterResourceId
output containerRegistryName string = foundation.outputs.containerRegistryName
output containerRegistryLoginServer string = foundation.outputs.containerRegistryLoginServer
output keyVaultName string = foundation.outputs.keyVaultName
output keyVaultUri string = foundation.outputs.keyVaultUri
output logAnalyticsWorkspaceId string = foundation.outputs.logAnalyticsWorkspaceId
output oidcIssuerUrl string = foundation.outputs.oidcIssuerUrl
output apiIdentityClientId string = foundation.outputs.apiIdentityClientId
output brokerIdentityClientId string = foundation.outputs.brokerIdentityClientId
output bootstrapIdentityClientId string = foundation.outputs.bootstrapIdentityClientId
output frontDoorProfileName string = foundation.outputs.frontDoorProfileName
output frontDoorProfileResourceId string = foundation.outputs.frontDoorProfileResourceId
output frontDoorProfileId string = foundation.outputs.frontDoorProfileId
output frontDoorApiEndpointName string = foundation.outputs.frontDoorApiEndpointName
output frontDoorApiHostname string = foundation.outputs.frontDoorApiHostname
output frontDoorWebEndpointName string = foundation.outputs.frontDoorWebEndpointName
output frontDoorWebHostname string = foundation.outputs.frontDoorWebHostname
output apiOriginHostname string = foundation.outputs.apiOriginHostname
output webOriginHostname string = foundation.outputs.webOriginHostname
