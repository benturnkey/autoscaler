package aws

import (
	"fmt"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type testRegionalServiceSet struct {
	AutoScaling map[string]*autoScalingMock
	EC2         map[string]*ec2Mock
	EKS         map[string]*eksMock
}

func newTestRegionalAWSService(regions ...string) (*staticAWSServiceRouter, *testRegionalServiceSet) {
	services := make(map[string]*awsWrapper, len(regions))
	mocks := &testRegionalServiceSet{
		AutoScaling: make(map[string]*autoScalingMock, len(regions)),
		EC2:         make(map[string]*ec2Mock, len(regions)),
		EKS:         make(map[string]*eksMock, len(regions)),
	}

	defaultRegion := ""
	if len(regions) > 0 {
		defaultRegion = regions[0]
	}

	for _, region := range regions {
		autoScaling := &autoScalingMock{}
		ec2Client := &ec2Mock{}
		eksClient := &eksMock{}

		mocks.AutoScaling[region] = autoScaling
		mocks.EC2[region] = ec2Client
		mocks.EKS[region] = eksClient
		services[region] = &awsWrapper{
			autoScalingI: autoScaling,
			ec2I:         ec2Client,
			eksI:         eksClient,
		}
	}

	return newStaticAWSServiceRouter(defaultRegion, services), mocks
}

func newTestRegionalAwsManager(t *testing.T, regions []string, autoDiscoverySpecs []asgAutoDiscoveryConfig) (*AwsManager, *testRegionalServiceSet) {
	t.Helper()

	serviceRouter, mocks := newTestRegionalAWSService(regions...)
	var defaultService *awsWrapper
	if len(regions) > 0 {
		var err error
		defaultService, err = serviceRouter.forRegion(regions[0])
		require.NoError(t, err)
	}

	cache, err := newASGCache(defaultService, nil, autoDiscoverySpecs)
	require.NoError(t, err)
	cache.awsServiceRouter = serviceRouter

	manager := &AwsManager{
		asgCache:              cache,
		awsServiceRouter:      serviceRouter,
		managedNodegroupCache: newManagedNodeGroupCache(defaultService),
	}
	if defaultService != nil {
		manager.awsService = *defaultService
	}
	manager.managedNodegroupCache.awsServiceRouter = serviceRouter

	return manager, mocks
}

func testRegionalDescribeAutoScalingGroupsOutput(groupName string, region string, desiredCap int32, instanceIDs ...string) *autoscaling.DescribeAutoScalingGroupsOutput {
	zone := fmt.Sprintf("%sa", region)
	instances := make([]autoscalingtypes.Instance, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		instances = append(instances, autoscalingtypes.Instance{
			InstanceId:       aws.String(id),
			AvailabilityZone: aws.String(zone),
			LifecycleState:   autoscalingtypes.LifecycleStateInService,
		})
	}

	return &autoscaling.DescribeAutoScalingGroupsOutput{
		AutoScalingGroups: []autoscalingtypes.AutoScalingGroup{
			{
				AutoScalingGroupName: aws.String(groupName),
				DesiredCapacity:      aws.Int32(desiredCap),
				MinSize:              aws.Int32(1),
				MaxSize:              aws.Int32(5),
				Instances:            instances,
				AvailabilityZones:    []string{zone},
			},
		},
	}
}

func TestAutoDiscoveredNodeGroupsAcrossRegions(t *testing.T) {
	manager, mocks := newTestRegionalAwsManager(t, []string{"us-east-1", "us-west-2"}, []asgAutoDiscoveryConfig{
		{Tags: map[string]string{"test": ""}},
	})
	provider := testProvider(t, manager)

	input := &autoscaling.DescribeAutoScalingGroupsInput{
		Filters: []autoscalingtypes.Filter{
			{Name: aws.String("tag-key"), Values: []string{"test"}},
		},
		MaxRecords: aws.Int32(maxRecordsReturnedByAPI),
	}
	mocks.AutoScaling["us-east-1"].On("DescribeAutoScalingGroups", mock.Anything, input).
		Return(testRegionalDescribeAutoScalingGroupsOutput("east-asg", "us-east-1", 1, "i-east"), nil)
	mocks.AutoScaling["us-west-2"].On("DescribeAutoScalingGroups", mock.Anything, input).
		Return(testRegionalDescribeAutoScalingGroupsOutput("west-asg", "us-west-2", 1, "i-west"), nil)

	require.NoError(t, provider.Refresh())

	nodeGroups := provider.NodeGroups()
	require.Len(t, nodeGroups, 2)

	ids := []string{nodeGroups[0].Id(), nodeGroups[1].Id()}
	sort.Strings(ids)
	assert.Equal(t, []string{"us-east-1/east-asg", "us-west-2/west-asg"}, ids)
	mocks.AutoScaling["us-east-1"].AssertNumberOfCalls(t, "DescribeAutoScalingGroups", 1)
	mocks.AutoScaling["us-west-2"].AssertNumberOfCalls(t, "DescribeAutoScalingGroups", 1)
}

func TestSetAsgSizeRoutesToRegionalService(t *testing.T) {
	manager, mocks := newTestRegionalAwsManager(t, []string{"us-east-1", "us-west-2"}, nil)

	testAsg := &asg{
		AwsRef:  AwsRef{Name: "west-asg", Region: "us-west-2"},
		curSize: 1,
		region:  "us-west-2",
	}
	mocks.AutoScaling["us-west-2"].On(
		"SetDesiredCapacity",
		mock.Anything,
		&autoscaling.SetDesiredCapacityInput{
			AutoScalingGroupName: aws.String("west-asg"),
			DesiredCapacity:      aws.Int32(3),
			HonorCooldown:        aws.Bool(false),
		},
	).Return(&autoscaling.SetDesiredCapacityOutput{}, nil)

	require.NoError(t, manager.SetAsgSize(testAsg, 3))
	assert.Equal(t, 3, testAsg.curSize)
	mocks.AutoScaling["us-west-2"].AssertNumberOfCalls(t, "SetDesiredCapacity", 1)
	mocks.AutoScaling["us-east-1"].AssertNumberOfCalls(t, "SetDesiredCapacity", 0)
}

func TestAutoDiscoveredNodeGroupsAcrossRegionsWithSameName(t *testing.T) {
	manager, mocks := newTestRegionalAwsManager(t, []string{"us-east-1", "us-west-2"}, []asgAutoDiscoveryConfig{
		{Tags: map[string]string{"test": ""}},
	})
	provider := testProvider(t, manager)

	input := &autoscaling.DescribeAutoScalingGroupsInput{
		Filters: []autoscalingtypes.Filter{
			{Name: aws.String("tag-key"), Values: []string{"test"}},
		},
		MaxRecords: aws.Int32(maxRecordsReturnedByAPI),
	}
	mocks.AutoScaling["us-east-1"].On("DescribeAutoScalingGroups", mock.Anything, input).
		Return(testRegionalDescribeAutoScalingGroupsOutput("shared-asg", "us-east-1", 1, "i-east"), nil)
	mocks.AutoScaling["us-west-2"].On("DescribeAutoScalingGroups", mock.Anything, input).
		Return(testRegionalDescribeAutoScalingGroupsOutput("shared-asg", "us-west-2", 1, "i-west"), nil)

	require.NoError(t, provider.Refresh())

	nodeGroups := provider.NodeGroups()
	require.Len(t, nodeGroups, 2)

	ids := []string{nodeGroups[0].Id(), nodeGroups[1].Id()}
	sort.Strings(ids)
	assert.Equal(t, []string{"us-east-1/shared-asg", "us-west-2/shared-asg"}, ids)
}

func TestIsNodeGroupAvailableRoutesToRegionalService(t *testing.T) {
	manager, mocks := newTestRegionalAwsManager(t, []string{"us-east-1", "us-west-2"}, nil)

	group := &autoscalingtypes.AutoScalingGroup{
		AutoScalingGroupName: aws.String("west-asg"),
		AvailabilityZones:    []string{"us-west-2a"},
	}
	mocks.AutoScaling["us-west-2"].On(
		"DescribeScalingActivities",
		mock.Anything,
		&autoscaling.DescribeScalingActivitiesInput{
			AutoScalingGroupName: aws.String("west-asg"),
		},
	).Return(&autoscaling.DescribeScalingActivitiesOutput{}, nil)

	available, err := manager.asgCache.isNodeGroupAvailable(group)
	require.NoError(t, err)
	assert.True(t, available)
	mocks.AutoScaling["us-west-2"].AssertNumberOfCalls(t, "DescribeScalingActivities", 1)
	mocks.AutoScaling["us-east-1"].AssertNumberOfCalls(t, "DescribeScalingActivities", 0)
}

func TestManagedNodegroupCacheSeparatesRegions(t *testing.T) {
	manager, mocks := newTestRegionalAwsManager(t, []string{"us-east-1", "us-west-2"}, nil)

	nodegroupName := "shared-ng"
	clusterName := "cluster-a"
	eastLabelKey := "region"
	eastLabelValue := "us-east-1"
	westLabelValue := "us-west-2"

	mocks.EKS["us-east-1"].On(
		"DescribeNodegroup",
		mock.Anything,
		&eks.DescribeNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(nodegroupName),
		},
	).Return(&eks.DescribeNodegroupOutput{
		Nodegroup: &ekstypes.Nodegroup{
			NodegroupName: aws.String(nodegroupName),
			ClusterName:   aws.String(clusterName),
			Labels:        map[string]string{eastLabelKey: eastLabelValue},
		},
	}, nil)

	mocks.EKS["us-west-2"].On(
		"DescribeNodegroup",
		mock.Anything,
		&eks.DescribeNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(nodegroupName),
		},
	).Return(&eks.DescribeNodegroupOutput{
		Nodegroup: &ekstypes.Nodegroup{
			NodegroupName: aws.String(nodegroupName),
			ClusterName:   aws.String(clusterName),
			Labels:        map[string]string{eastLabelKey: westLabelValue},
		},
	}, nil)

	eastLabels, err := manager.managedNodegroupCache.getManagedNodegroupLabelsForRegion(nodegroupName, clusterName, "us-east-1")
	require.NoError(t, err)
	westLabels, err := manager.managedNodegroupCache.getManagedNodegroupLabelsForRegion(nodegroupName, clusterName, "us-west-2")
	require.NoError(t, err)

	assert.Equal(t, eastLabelValue, eastLabels[eastLabelKey])
	assert.Equal(t, westLabelValue, westLabels[eastLabelKey])
	mocks.EKS["us-east-1"].AssertNumberOfCalls(t, "DescribeNodegroup", 1)
	mocks.EKS["us-west-2"].AssertNumberOfCalls(t, "DescribeNodegroup", 1)
}
