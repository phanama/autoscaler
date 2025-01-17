/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2019-07-01/compute"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
)

func newTestScaleSet(manager *AzureManager, name string) *ScaleSet {
	return &ScaleSet{
		azureRef: azureRef{
			Name: name,
		},
		manager:           manager,
		minSize:           1,
		maxSize:           5,
		sizeRefreshPeriod: defaultVmssSizeRefreshPeriod,
	}
}

func TestMaxSize(t *testing.T) {
	provider := newTestProvider(t)
	registered := provider.azureManager.RegisterAsg(
		newTestScaleSet(provider.azureManager, "test-asg"))
	assert.True(t, registered)
	assert.Equal(t, len(provider.NodeGroups()), 1)
	assert.Equal(t, provider.NodeGroups()[0].MaxSize(), 5)
}

func TestMinSize(t *testing.T) {
	provider := newTestProvider(t)
	registered := provider.azureManager.RegisterAsg(
		newTestScaleSet(provider.azureManager, "test-asg"))
	assert.True(t, registered)
	assert.Equal(t, len(provider.NodeGroups()), 1)
	assert.Equal(t, provider.NodeGroups()[0].MinSize(), 1)
}

func TestTargetSize(t *testing.T) {
	provider := newTestProvider(t)
	registered := provider.azureManager.RegisterAsg(
		newTestScaleSet(provider.azureManager, "test-asg"))
	assert.True(t, registered)
	assert.Equal(t, len(provider.NodeGroups()), 1)

	targetSize, err := provider.NodeGroups()[0].TargetSize()
	assert.NoError(t, err)
	assert.Equal(t, 3, targetSize)
}

func TestIncreaseSize(t *testing.T) {
	provider := newTestProvider(t)
	registered := provider.azureManager.RegisterAsg(
		newTestScaleSet(provider.azureManager, "test-asg"))
	assert.True(t, registered)
	assert.Equal(t, len(provider.NodeGroups()), 1)

	// current target size is 2.
	targetSize, err := provider.NodeGroups()[0].TargetSize()
	assert.NoError(t, err)
	assert.Equal(t, 3, targetSize)

	// increase 3 nodes.
	err = provider.NodeGroups()[0].IncreaseSize(2)
	assert.NoError(t, err)

	// new target size should be 5.
	targetSize, err = provider.NodeGroups()[0].TargetSize()
	assert.NoError(t, err)
	assert.Equal(t, 5, targetSize)
}

func TestIncreaseSizeOnVMSSUpdating(t *testing.T) {
	manager := newTestAzureManager(t)
	vmssName := "vmss-updating"
	var vmssCapacity int64 = 3
	scaleSetClient := &VirtualMachineScaleSetsClientMock{
		FakeStore: map[string]map[string]compute.VirtualMachineScaleSet{
			"test": {
				vmssName: {
					Name: &vmssName,
					Sku: &compute.Sku{
						Capacity: &vmssCapacity,
					},
					VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
						ProvisioningState: to.StringPtr(string(compute.ProvisioningStateUpdating)),
					},
				},
			},
		},
	}
	manager.azClient.virtualMachineScaleSetsClient = scaleSetClient
	registered := manager.RegisterAsg(newTestScaleSet(manager, vmssName))
	assert.True(t, registered)
	manager.regenerateCache()

	provider, err := BuildAzureCloudProvider(manager, nil)
	assert.NoError(t, err)

	// Scaling should continue even VMSS is under updating.
	scaleSet, ok := provider.NodeGroups()[0].(*ScaleSet)
	assert.True(t, ok)
	err = scaleSet.IncreaseSize(1)
	assert.NoError(t, err)
}

func TestBelongs(t *testing.T) {
	provider := newTestProvider(t)
	registered := provider.azureManager.RegisterAsg(
		newTestScaleSet(provider.azureManager, "test-asg"))
	assert.True(t, registered)

	scaleSet, ok := provider.NodeGroups()[0].(*ScaleSet)
	assert.True(t, ok)
	// TODO: this should call manager.Refresh() once the fetchAutoASG
	// logic is refactored out
	provider.azureManager.regenerateCache()

	invalidNode := &apiv1.Node{
		Spec: apiv1.NodeSpec{
			ProviderID: "azure:///subscriptions/test-subscrition-id/resourcegroups/invalid-asg/providers/microsoft.compute/virtualmachinescalesets/agents/virtualmachines/0",
		},
	}
	_, err := scaleSet.Belongs(invalidNode)
	assert.Error(t, err)

	validNode := &apiv1.Node{
		Spec: apiv1.NodeSpec{
			ProviderID: "azure://" + fakeVirtualMachineScaleSetVMID,
		},
	}
	belongs, err := scaleSet.Belongs(validNode)
	assert.Equal(t, true, belongs)
	assert.NoError(t, err)
}

func TestDeleteNodes(t *testing.T) {
	manager := newTestAzureManager(t)
	vmssName := "test-asg"
	var vmssCapacity int64 = 3
	scaleSetClient := &VirtualMachineScaleSetsClientMock{
		FakeStore: map[string]map[string]compute.VirtualMachineScaleSet{
			"test": {
				"test-asg": {
					Name: &vmssName,
					Sku: &compute.Sku{
						Capacity: &vmssCapacity,
					},
				},
			},
		},
	}
	response := autorest.Response{
		Response: &http.Response{
			Status: "OK",
		},
	}
	scaleSetClient.On("DeleteInstancesAsync", mock.Anything, "test-asg", mock.Anything, mock.Anything).Return(response, nil)
	manager.azClient.virtualMachineScaleSetsClient = scaleSetClient
	// TODO: this should call manager.Refresh() once the fetchAutoASG
	// logic is refactored out
	manager.regenerateCache()

	resourceLimiter := cloudprovider.NewResourceLimiter(
		map[string]int64{cloudprovider.ResourceNameCores: 1, cloudprovider.ResourceNameMemory: 10000000},
		map[string]int64{cloudprovider.ResourceNameCores: 10, cloudprovider.ResourceNameMemory: 100000000})
	provider, err := BuildAzureCloudProvider(manager, resourceLimiter)
	assert.NoError(t, err)

	registered := manager.RegisterAsg(
		newTestScaleSet(manager, "test-asg"))
	assert.True(t, registered)
	// TODO: this should call manager.Refresh() once the fetchAutoASG
	// logic is refactored out
	manager.regenerateCache()

	node := &apiv1.Node{
		Spec: apiv1.NodeSpec{
			ProviderID: "azure://" + fakeVirtualMachineScaleSetVMID,
		},
	}
	scaleSet, ok := provider.NodeGroups()[0].(*ScaleSet)
	assert.True(t, ok)

	targetSize, err := scaleSet.TargetSize()
	assert.NoError(t, err)
	assert.Equal(t, 3, targetSize)

	// Perform the delete operation
	err = scaleSet.DeleteNodes([]*apiv1.Node{node})
	assert.NoError(t, err)

	// Ensure the the cached size has been proactively decremented
	targetSize, err = scaleSet.TargetSize()
	assert.NoError(t, err)
	assert.Equal(t, 2, targetSize)

	scaleSetClient.AssertNumberOfCalls(t, "DeleteInstancesAsync", 1)
}

func TestDeleteNoConflictRequest(t *testing.T) {
	vmssName := "test-asg"
	var vmssCapacity int64 = 3

	manager := newTestAzureManager(t)
	vmsClient := &VirtualMachineScaleSetVMsClientMock{
		FakeStore: map[string]map[string]compute.VirtualMachineScaleSetVM{
			"test": {
				"0": {
					ID:         to.StringPtr(fakeVirtualMachineScaleSetVMID),
					InstanceID: to.StringPtr("0"),
					VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
						VMID:              to.StringPtr("123E4567-E89B-12D3-A456-426655440000"),
						ProvisioningState: to.StringPtr("Deleting"),
					},
				},
			},
		},
	}

	scaleSetClient := &VirtualMachineScaleSetsClientMock{
		FakeStore: map[string]map[string]compute.VirtualMachineScaleSet{
			"test": {
				"test-asg": {
					Name: &vmssName,
					Sku: &compute.Sku{
						Capacity: &vmssCapacity,
					},
				},
			},
		},
	}

	response := autorest.Response{
		Response: &http.Response{
			Status: "OK",
		},
	}

	scaleSetClient.On("DeleteInstancesAsync", mock.Anything, "test-asg", mock.Anything, mock.Anything).Return(response, nil)
	manager.azClient.virtualMachineScaleSetsClient = scaleSetClient
	manager.azClient.virtualMachineScaleSetVMsClient = vmsClient

	resourceLimiter := cloudprovider.NewResourceLimiter(
		map[string]int64{cloudprovider.ResourceNameCores: 1, cloudprovider.ResourceNameMemory: 10000000},
		map[string]int64{cloudprovider.ResourceNameCores: 10, cloudprovider.ResourceNameMemory: 100000000})
	provider, err := BuildAzureCloudProvider(manager, resourceLimiter)
	assert.NoError(t, err)

	registered := manager.RegisterAsg(newTestScaleSet(manager, "test-asg"))
	assert.True(t, registered)

	node := &apiv1.Node{
		Spec: apiv1.NodeSpec{
			ProviderID: "azure://" + fakeVirtualMachineScaleSetVMID,
		},
	}

	scaleSet, ok := provider.NodeGroups()[0].(*ScaleSet)
	assert.True(t, ok)

	err = scaleSet.DeleteNodes([]*apiv1.Node{node})
	// ensure that DeleteInstancesAsync isn't called
	scaleSetClient.AssertNumberOfCalls(t, "DeleteInstancesAsync", 0)
}

func TestId(t *testing.T) {
	provider := newTestProvider(t)
	registered := provider.azureManager.RegisterAsg(
		newTestScaleSet(provider.azureManager, "test-asg"))
	assert.True(t, registered)
	assert.Equal(t, len(provider.NodeGroups()), 1)
	assert.Equal(t, provider.NodeGroups()[0].Id(), "test-asg")
}

func TestDebug(t *testing.T) {
	asg := ScaleSet{
		manager: newTestAzureManager(t),
		minSize: 5,
		maxSize: 55,
	}
	asg.Name = "test-scale-set"
	assert.Equal(t, asg.Debug(), "test-scale-set (5:55)")
}

func TestScaleSetNodes(t *testing.T) {
	provider := newTestProvider(t)
	registered := provider.azureManager.RegisterAsg(
		newTestScaleSet(provider.azureManager, "test-asg"))
	// TODO: this should call manager.Refresh() once the fetchAutoASG
	// logic is refactored out
	provider.azureManager.regenerateCache()
	assert.True(t, registered)
	assert.Equal(t, len(provider.NodeGroups()), 1)

	fakeProviderID := "azure://" + fakeVirtualMachineScaleSetVMID
	node := &apiv1.Node{
		Spec: apiv1.NodeSpec{
			ProviderID: fakeProviderID,
		},
	}
	group, err := provider.NodeGroupForNode(node)
	assert.NoError(t, err)
	assert.NotNil(t, group, "Group should not be nil")
	assert.Equal(t, group.Id(), "test-asg")
	assert.Equal(t, group.MinSize(), 1)
	assert.Equal(t, group.MaxSize(), 5)

	ss, ok := group.(*ScaleSet)
	assert.True(t, ok)
	assert.NotNil(t, ss)
	instances, err := group.Nodes()
	assert.NoError(t, err)
	assert.Equal(t, len(instances), 1)
	assert.Equal(t, instances[0], cloudprovider.Instance{Id: fakeProviderID})
}

func TestTemplateNodeInfo(t *testing.T) {
	provider := newTestProvider(t)
	registered := provider.azureManager.RegisterAsg(
		newTestScaleSet(provider.azureManager, "test-asg"))
	assert.True(t, registered)
	assert.Equal(t, len(provider.NodeGroups()), 1)

	asg := ScaleSet{
		manager: newTestAzureManager(t),
		minSize: 1,
		maxSize: 5,
	}
	asg.Name = "test-asg"

	nodeInfo, err := asg.TemplateNodeInfo()
	assert.NoError(t, err)
	assert.NotNil(t, nodeInfo)
	assert.NotEmpty(t, nodeInfo.Pods())
}

func TestExtractAllocatableResourcesFromScaleSet(t *testing.T) {
	tags := map[string]*string{
		fmt.Sprintf("%s%s", nodeResourcesTagName, "cpu"):               to.StringPtr("100m"),
		fmt.Sprintf("%s%s", nodeResourcesTagName, "memory"):            to.StringPtr("100M"),
		fmt.Sprintf("%s%s", nodeResourcesTagName, "ephemeral-storage"): to.StringPtr("20G"),
	}

	labels := extractAllocatableResourcesFromScaleSet(tags)

	assert.Equal(t, resource.NewMilliQuantity(100, resource.DecimalSI).String(), labels["cpu"].String())
	expectedMemory := resource.MustParse("100M")
	assert.Equal(t, (&expectedMemory).String(), labels["memory"].String())
	expectedEphemeralStorage := resource.MustParse("20G")
	assert.Equal(t, (&expectedEphemeralStorage).String(), labels["ephemeral-storage"].String())
}
