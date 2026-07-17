/*
Copyright 2026 The Kubernetes Authors.

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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type fsxClient interface {
	DescribeFileSystems(ctx context.Context, params *fsx.DescribeFileSystemsInput, optFns ...func(*fsx.Options)) (*fsx.DescribeFileSystemsOutput, error)
	DeleteFileSystem(ctx context.Context, params *fsx.DeleteFileSystemInput, optFns ...func(*fsx.Options)) (*fsx.DeleteFileSystemOutput, error)
}

// FSxLustreFileSystems manages only FSx for Lustre file systems. Other FSx
// families have different deletion prerequisites and backup behavior.
type FSxLustreFileSystems struct{}

func (FSxLustreFileSystems) MarkAndSweep(opts Options, set *Set) error {
	svc := fsx.NewFromConfig(*opts.Config, func(opt *fsx.Options) {
		opt.Region = opts.Region
	})
	return markAndSweepFSxLustreFileSystems(svc, opts, set)
}

func markAndSweepFSxLustreFileSystems(svc fsxClient, opts Options, set *Set) error {
	logger := logrus.WithField("options", opts)
	fileSystems, err := describeFSxFileSystems(svc)
	if err != nil {
		return errors.Wrapf(err, "couldn't describe FSx file systems for %q in %q", opts.Account, opts.Region)
	}

	// Finish discovery before issuing deletions so a pagination failure cannot
	// leave the account only partially swept.
	toDelete := make([]*fsxLustreFileSystem, 0, len(fileSystems))
	for _, description := range fileSystems {
		if description.FileSystemType != fsxtypes.FileSystemTypeLustre {
			continue
		}

		fileSystem, err := newFSxLustreFileSystem(description)
		if err != nil {
			return err
		}
		tags, err := fromFSxTags(description.Tags)
		if err != nil {
			return fmt.Errorf("%s: invalid tags: %w", fileSystem.ARN(), err)
		}
		if !set.Mark(opts, fileSystem, description.CreationTime, tags) {
			continue
		}
		if description.Lifecycle == fsxtypes.FileSystemLifecycleDeleting {
			logger.Infof("%s: already deleting", fileSystem.ARN())
			continue
		}

		logger.Warningf("%s: deleting %T: %s", fileSystem.ARN(), description, fileSystem.id)
		if !opts.DryRun {
			toDelete = append(toDelete, fileSystem)
		}
	}

	for _, fileSystem := range toDelete {
		input := &fsx.DeleteFileSystemInput{
			FileSystemId: aws.String(fileSystem.id),
			LustreConfiguration: &fsxtypes.DeleteFileSystemLustreConfiguration{
				SkipFinalBackup: aws.Bool(true),
			},
		}
		if _, err := svc.DeleteFileSystem(context.TODO(), input); err != nil {
			logger.Warningf("%s: delete failed: %v", fileSystem.ARN(), err)
		}
	}

	return nil
}

func (FSxLustreFileSystems) ListAll(opts Options) (*Set, error) {
	svc := fsx.NewFromConfig(*opts.Config, func(opt *fsx.Options) {
		opt.Region = opts.Region
	})
	return listAllFSxLustreFileSystems(svc, opts)
}

func listAllFSxLustreFileSystems(svc fsxClient, opts Options) (*Set, error) {
	set := NewSet(0)
	fileSystems, err := describeFSxFileSystems(svc)
	if err != nil {
		return set, errors.Wrapf(err, "couldn't describe FSx file systems for %q in %q", opts.Account, opts.Region)
	}

	now := time.Now()
	for _, description := range fileSystems {
		if description.FileSystemType != fsxtypes.FileSystemTypeLustre {
			continue
		}
		fileSystem, err := newFSxLustreFileSystem(description)
		if err != nil {
			return set, err
		}
		set.firstSeen[fileSystem.ResourceKey()] = now
	}

	return set, nil
}

func describeFSxFileSystems(svc fsxClient) ([]fsxtypes.FileSystem, error) {
	paginator := fsx.NewDescribeFileSystemsPaginator(svc, &fsx.DescribeFileSystemsInput{})
	var fileSystems []fsxtypes.FileSystem

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.TODO())
		if err != nil {
			return nil, err
		}
		fileSystems = append(fileSystems, page.FileSystems...)
	}

	return fileSystems, nil
}

func newFSxLustreFileSystem(description fsxtypes.FileSystem) (*fsxLustreFileSystem, error) {
	if description.FileSystemId == nil || *description.FileSystemId == "" {
		return nil, errors.New("service returned a Lustre file system without an ID")
	}
	if description.ResourceARN == nil || *description.ResourceARN == "" {
		return nil, fmt.Errorf("file system %q returned by FSx has no ARN", *description.FileSystemId)
	}

	return &fsxLustreFileSystem{
		arn: *description.ResourceARN,
		id:  *description.FileSystemId,
	}, nil
}

func fromFSxTags(fsxTags []fsxtypes.Tag) (Tags, error) {
	tags := make(Tags, len(fsxTags))
	for i, tag := range fsxTags {
		if tag.Key == nil || *tag.Key == "" {
			return nil, fmt.Errorf("tag %d has no key", i)
		}
		tags[*tag.Key] = aws.ToString(tag.Value)
	}
	return tags, nil
}

type fsxLustreFileSystem struct {
	arn string
	id  string
}

func (fileSystem fsxLustreFileSystem) ARN() string {
	return fileSystem.arn
}

func (fileSystem fsxLustreFileSystem) ResourceKey() string {
	return fileSystem.ARN()
}
