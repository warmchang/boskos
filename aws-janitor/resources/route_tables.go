/*
Copyright 2019 The Kubernetes Authors.

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

package resources

import (
	"context"
	"fmt"
	"time"

	ec2v2 "github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// RouteTables: https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#EC2.DescribeRouteTables

type RouteTables struct{}

func (RouteTables) MarkAndSweep(opts Options, set *Set) error {
	logger := logrus.WithField("options", opts)
	svc := ec2v2.NewFromConfig(*opts.Config, func(opt *ec2v2.Options) {
		opt.Region = opts.Region
	})

	resp, err := svc.DescribeRouteTables(context.TODO(), nil)
	if err != nil {
		return err
	}

	for _, rt := range resp.RouteTables {
		// Filter out the RouteTables that have a main
		// association. Given the documentation for the main.association
		// filter, you'd think we could filter on the Describe, but it
		// doesn't actually work, see e.g.
		// https://github.com/aws/aws-cli/issues/1810
		main := false
		for _, assoc := range rt.Associations {
			main = main || *assoc.Main
		}
		if main {
			continue
		}

		r := &routeTable{Account: opts.Account, Region: opts.Region, ID: *rt.RouteTableId}
		tags := fromEC2Tags(rt.Tags)
		if !set.Mark(opts, r, nil, tags) {
			continue
		}
		logger.Warningf("%s: deleting %T: %s (%s)", r.ARN(), rt, r.ID, tags[NameTagKey])
		if opts.DryRun {
			continue
		}

		for _, assoc := range rt.Associations {
			logger.Infof("%s: disassociating from %s", r.ARN(), *assoc.SubnetId)

			disReq := &ec2v2.DisassociateRouteTableInput{
				AssociationId: assoc.RouteTableAssociationId,
			}

			if _, err := svc.DisassociateRouteTable(context.TODO(), disReq); err != nil {
				logger.Warningf("%s: disassociation from subnet %s failed: %v", r.ARN(), *assoc.SubnetId, err)
			}
		}

		deleteReq := &ec2v2.DeleteRouteTableInput{
			RouteTableId: rt.RouteTableId,
		}

		if _, err := svc.DeleteRouteTable(context.TODO(), deleteReq); err != nil {
			logger.Warningf("%s: delete failed: %v", r.ARN(), err)
		}
	}

	return nil
}

func (RouteTables) ListAll(opts Options) (*Set, error) {
	svc := ec2v2.NewFromConfig(*opts.Config, func(opt *ec2v2.Options) {
		opt.Region = opts.Region
	})
	set := NewSet(0)
	input := &ec2v2.DescribeRouteTablesInput{}

	err := DescribeRouteTablesPages(svc, input, func(tables *ec2v2.DescribeRouteTablesOutput, _ bool) bool {
		now := time.Now()
		for _, table := range tables.RouteTables {
			arn := routeTable{
				Account: opts.Account,
				Region:  opts.Region,
				ID:      *table.RouteTableId,
			}.ARN()
			set.firstSeen[arn] = now
		}

		return true
	})

	return set, errors.Wrapf(err, "couldn't describe route tables for %q in %q", opts.Account, opts.Region)
}

func DescribeRouteTablesPages(svc *ec2v2.Client, input *ec2v2.DescribeRouteTablesInput, pageFunc func(tables *ec2v2.DescribeRouteTablesOutput, _ bool) bool) error {
	paginator := ec2v2.NewDescribeRouteTablesPaginator(svc, input)

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.TODO())
		if err != nil {
			logrus.Warningf("failed to get page, %v", err)
		} else {
			pageFunc(page, false)
		}
	}
	return nil
}

type routeTable struct {
	Account string
	Region  string
	ID      string
}

func (rt routeTable) ARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:route-table/%s", rt.Region, rt.Account, rt.ID)
}

func (rt routeTable) ResourceKey() string {
	return rt.ARN()
}
