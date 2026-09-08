using '../foundation.bicep'

param resourcePrefix = 'devsbx'
param location = 'westeurope'
param environmentName = 'poc'
param deploymentPrincipalObjectId = ''
param vnetAddressPrefix = '10.240.0.0/16'
param aksSubnetAddressPrefix = '10.240.0.0/20'
param systemNodeVmSize = 'Standard_D4s_v6'
param systemNodeCount = 2
param kataNodeVmSize = 'Standard_D16s_v6'
param kataMinCount = 1
param kataMaxCount = 10
param tags = {
  costCenter: 'devsandbox-poc'
  dataClassification: 'internal'
}
