targetScope = 'resourceGroup'

param location string
param vnetName string
param vnetAddressPrefix string
param aksSubnetAddressPrefix string
param tags object

var aksSubnetName = 'aks-nodes'

resource aksSubnetNsg 'Microsoft.Network/networkSecurityGroups@2025-01-01' = {
  name: '${vnetName}-${aksSubnetName}-nsg-${location}'
  location: location
  tags: tags
  properties: {
    securityRules: [
      {
        name: 'AllowFrontDoorHttpInbound'
        properties: {
          access: 'Allow'
          destinationAddressPrefix: '*'
          destinationPortRange: '80'
          direction: 'Inbound'
          priority: 100
          protocol: 'Tcp'
          sourceAddressPrefix: 'AzureFrontDoor.Backend'
          sourcePortRange: '*'
        }
      }
      {
        name: 'AllowAzureLoadBalancerHealthInbound'
        properties: {
          access: 'Allow'
          destinationAddressPrefix: '*'
          destinationPortRange: '*'
          direction: 'Inbound'
          priority: 110
          protocol: '*'
          sourceAddressPrefix: 'AzureLoadBalancer'
          sourcePortRange: '*'
        }
      }
    ]
  }
}

resource vnet 'Microsoft.Network/virtualNetworks@2025-01-01' = {
  name: vnetName
  location: location
  tags: tags
  properties: {
    addressSpace: {
      addressPrefixes: [
        vnetAddressPrefix
      ]
    }
    subnets: [
      {
        name: aksSubnetName
        properties: {
          addressPrefix: aksSubnetAddressPrefix
          networkSecurityGroup: {
            id: aksSubnetNsg.id
          }
          privateEndpointNetworkPolicies: 'Disabled'
        }
      }
    ]
  }
}

output vnetResourceId string = vnet.id
output aksSubnetName string = aksSubnetName
output aksSubnetResourceId string = resourceId('Microsoft.Network/virtualNetworks/subnets', vnet.name, aksSubnetName)
