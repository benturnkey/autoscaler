package aws

import "fmt"

type awsServiceRouter interface {
	forRegion(region string) (*awsWrapper, error)
	regions() []string
}

type staticAWSServiceRouter struct {
	defaultRegion string
	services      map[string]*awsWrapper
}

func newSingleAWSServiceRouter(region string, service *awsWrapper) *staticAWSServiceRouter {
	services := map[string]*awsWrapper{}
	if service != nil {
		services[region] = service
	}
	return &staticAWSServiceRouter{
		defaultRegion: region,
		services:      services,
	}
}

func newStaticAWSServiceRouter(defaultRegion string, services map[string]*awsWrapper) *staticAWSServiceRouter {
	return &staticAWSServiceRouter{
		defaultRegion: defaultRegion,
		services:      services,
	}
}

func (r *staticAWSServiceRouter) forRegion(region string) (*awsWrapper, error) {
	if len(r.services) == 0 {
		return nil, fmt.Errorf("no AWS service configured")
	}
	if region == "" {
		region = r.defaultRegion
	}
	service, found := r.services[region]
	if !found {
		return nil, fmt.Errorf("no AWS service configured for region %q", region)
	}
	return service, nil
}

func (r *staticAWSServiceRouter) regions() []string {
	out := make([]string, 0, len(r.services))
	for region := range r.services {
		out = append(out, region)
	}
	return out
}
