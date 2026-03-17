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

package aws

import (
	"fmt"
	"sync"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/cache"
	klog "k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const (
	managedNodegroupCachedTTL = time.Minute * 6
	mngCacheMinTTL            = 5
	mngCacheMaxTTL            = 60
)

// managedNodegroupCache caches the managed nodegroup information.
// The store expires its keys based on a TTL. This TTL can have a jitter applied to it.
// This allows to get a better repartition of the AWS queries.
type managedNodegroupCache struct {
	cache.Store
	mngJitterClock   clock.Clock
	awsService       *awsWrapper
	awsServiceRouter awsServiceRouter
}

// This struct will be used to hold some information from the describeNodegroup call
// There are more options that can be added in the future
// https://docs.aws.amazon.com/cli/latest/reference/eks/describe-nodegroup.html
type managedNodegroupCachedObject struct {
	region      string
	name        string
	clusterName string
	taints      []apiv1.Taint
	labels      map[string]string
	tags        map[string]string
}

type mngJitterClock struct {
	clock.Clock

	jitter bool
	sync.RWMutex
}

func newManagedNodeGroupCache(awsService *awsWrapper) *managedNodegroupCache {
	jc := &mngJitterClock{}
	return newManagedNodeGroupCacheWithClock(
		awsService,
		jc,
		cache.NewExpirationStore(func(obj interface{}) (s string, e error) {
			return managedNodegroupCacheKey(
				obj.(managedNodegroupCachedObject).region,
				obj.(managedNodegroupCachedObject).clusterName,
				obj.(managedNodegroupCachedObject).name,
			), nil
		}, &cache.TTLPolicy{
			TTL:   managedNodegroupCachedTTL,
			Clock: jc,
		}),
	)
}

func newManagedNodeGroupCacheWithClock(awsService *awsWrapper, jc clock.Clock, store cache.Store) *managedNodegroupCache {
	return &managedNodegroupCache{
		Store:            store,
		mngJitterClock:   jc,
		awsService:       awsService,
		awsServiceRouter: nil,
	}
}

func (c *mngJitterClock) Since(ts time.Time) time.Duration {
	since := time.Since(ts)
	c.RLock()
	defer c.RUnlock()
	if c.jitter {
		return since + (time.Second * time.Duration(rand.IntnRange(mngCacheMinTTL, mngCacheMaxTTL)))
	}
	return since
}

func managedNodegroupCacheKey(region string, clusterName string, nodegroupName string) string {
	if region == "" {
		return nodegroupName
	}
	return fmt.Sprintf("%s/%s/%s", region, clusterName, nodegroupName)
}

func (m *managedNodegroupCache) getAWSServiceForRegion(region string) (*awsWrapper, error) {
	if m.awsServiceRouter != nil {
		return m.awsServiceRouter.forRegion(region)
	}
	if m.awsService == nil {
		return nil, fmt.Errorf("no AWS service configured")
	}
	return m.awsService, nil
}

func (m *managedNodegroupCache) getManagedNodegroupForRegion(nodegroupName string, clusterName string, region string) (*managedNodegroupCachedObject, error) {
	awsService, err := m.getAWSServiceForRegion(region)
	if err != nil {
		return nil, err
	}
	taintList, labelMap, tagMap, err := awsService.getManagedNodegroupInfo(nodegroupName, clusterName)
	if err != nil {
		// If there's an error cache an empty nodegroup to limit failed calls to the EKS API
		newEmptyNodegroup := managedNodegroupCachedObject{
			region:      region,
			name:        nodegroupName,
			clusterName: clusterName,
			taints:      nil,
			labels:      nil,
			tags:        nil,
		}

		m.Add(newEmptyNodegroup)
		return nil, err
	}

	newNodegroup := managedNodegroupCachedObject{
		region:      region,
		name:        nodegroupName,
		clusterName: clusterName,
		taints:      taintList,
		labels:      labelMap,
		tags:        tagMap,
	}

	m.Add(newNodegroup)

	return &newNodegroup, nil
}

func (m *managedNodegroupCache) getManagedNodegroup(nodegroupName string, clusterName string) (*managedNodegroupCachedObject, error) {
	return m.getManagedNodegroupForRegion(nodegroupName, clusterName, "")
}

func (m managedNodegroupCache) getManagedNodegroupInfoObjectForRegion(nodegroupName string, clusterName string, region string) (*managedNodegroupCachedObject, error) {
	// List expires old entries
	cacheList := m.List()
	klog.V(5).Infof("Current ManagedNodegroup cache: %+v\n", cacheList)

	cacheKey := managedNodegroupCacheKey(region, clusterName, nodegroupName)
	if obj, found, err := m.GetByKey(cacheKey); err == nil && found {
		foundNodeGroup := obj.(managedNodegroupCachedObject)
		return &foundNodeGroup, nil
	}

	managedNodegroupInfo, err := m.getManagedNodegroupForRegion(nodegroupName, clusterName, region)
	if err != nil {
		klog.Errorf("Failed to query the managed nodegroup %s for the cluster %s while looking for labels/taints/tags: %v", nodegroupName, clusterName, err)
		return nil, err
	}
	return managedNodegroupInfo, nil
}

func (m managedNodegroupCache) getManagedNodegroupInfoObject(nodegroupName string, clusterName string) (*managedNodegroupCachedObject, error) {
	return m.getManagedNodegroupInfoObjectForRegion(nodegroupName, clusterName, "")
}

func (m managedNodegroupCache) getManagedNodegroupLabelsForRegion(nodegroupName string, clusterName string, region string) (map[string]string, error) {
	getManagedNodegroupInfoObject, err := m.getManagedNodegroupInfoObjectForRegion(nodegroupName, clusterName, region)
	if err != nil {
		return nil, err
	}

	return getManagedNodegroupInfoObject.labels, nil
}

func (m managedNodegroupCache) getManagedNodegroupLabels(nodegroupName string, clusterName string) (map[string]string, error) {
	return m.getManagedNodegroupLabelsForRegion(nodegroupName, clusterName, "")
}

func (m managedNodegroupCache) getManagedNodegroupTagsForRegion(nodegroupName string, clusterName string, region string) (map[string]string, error) {
	getManagedNodegroupInfoObject, err := m.getManagedNodegroupInfoObjectForRegion(nodegroupName, clusterName, region)
	if err != nil {
		return nil, err
	}

	return getManagedNodegroupInfoObject.tags, nil
}

func (m managedNodegroupCache) getManagedNodegroupTags(nodegroupName string, clusterName string) (map[string]string, error) {
	return m.getManagedNodegroupTagsForRegion(nodegroupName, clusterName, "")
}

func (m managedNodegroupCache) getManagedNodegroupTaintsForRegion(nodegroupName string, clusterName string, region string) ([]apiv1.Taint, error) {
	getManagedNodegroupInfoObject, err := m.getManagedNodegroupInfoObjectForRegion(nodegroupName, clusterName, region)
	if err != nil {
		return nil, err
	}

	return getManagedNodegroupInfoObject.taints, nil
}

func (m managedNodegroupCache) getManagedNodegroupTaints(nodegroupName string, clusterName string) ([]apiv1.Taint, error) {
	return m.getManagedNodegroupTaintsForRegion(nodegroupName, clusterName, "")
}
