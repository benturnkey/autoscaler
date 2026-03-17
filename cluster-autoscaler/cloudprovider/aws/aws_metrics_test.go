package aws

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestObserveAWSRequestIncludesRegionLabel(t *testing.T) {
	requestSummary.Reset()

	observeAWSRequest("DescribeAutoScalingGroupsPages", nil, time.Now().Add(-100*time.Millisecond), "us-west-2")

	count := testutil.CollectAndCount(requestSummary)
	assert.Equal(t, 1, count)
}
