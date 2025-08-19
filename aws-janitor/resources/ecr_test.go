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
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

type mockECRClient struct {
	repositories []types.Repository
	images       map[string][]types.ImageDetail
	deletedCount int
}

func (m *mockECRClient) DescribeRepositories(ctx context.Context, params *ecr.DescribeRepositoriesInput, optFns ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error) {
	return &ecr.DescribeRepositoriesOutput{
		Repositories: m.repositories,
	}, nil
}

func (m *mockECRClient) DescribeImages(ctx context.Context, params *ecr.DescribeImagesInput, optFns ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error) {
	images := m.images[*params.RepositoryName]
	return &ecr.DescribeImagesOutput{
		ImageDetails: images,
	}, nil
}

func (m *mockECRClient) BatchDeleteImage(ctx context.Context, params *ecr.BatchDeleteImageInput, optFns ...func(*ecr.Options)) (*ecr.BatchDeleteImageOutput, error) {
	m.deletedCount += len(params.ImageIds)
	return &ecr.BatchDeleteImageOutput{
		Failures: []types.ImageFailure{},
	}, nil
}

func TestMarkAndSweepWithClient(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-48 * time.Hour)

	grid := []struct {
		name          string
		dryRun        bool
		repositories  []types.Repository
		images        map[string][]types.ImageDetail
		cleanRepos    []string
		imageCount    int
		newImageCount int
		expectedCount int
	}{
		{
			name:          "deletes old images",
			dryRun:        false,
			imageCount:    1,
			expectedCount: 1,
		},
		{
			name:          "dry run does not delete",
			dryRun:        true,
			imageCount:    1,
			expectedCount: 0,
		},
		{
			name:          "batches deletion for >100 images",
			dryRun:        false,
			imageCount:    150,
			expectedCount: 150,
		},
		{
			name:          "deletes only images older than cutoff",
			dryRun:        false,
			imageCount:    5,
			newImageCount: 3,
			expectedCount: 5,
		},
		{
			name:   "skips images in non-configured repos",
			dryRun: false,
			repositories: []types.Repository{
				{
					RegistryId:     aws.String("123456789"),
					RepositoryName: aws.String("test-repo"),
					RepositoryArn:  aws.String("arn:aws:ecr:us-west-2:123456789:repository/test-repo"),
				},
				{
					RegistryId:     aws.String("123456789"),
					RepositoryName: aws.String("other-repo"),
					RepositoryArn:  aws.String("arn:aws:ecr:us-west-2:123456789:repository/other-repo"),
				},
			},
			images: map[string][]types.ImageDetail{
				"test-repo": {
					{
						RegistryId:     aws.String("123456789"),
						RepositoryName: aws.String("test-repo"),
						ImageDigest:    aws.String("sha256:abc123"),
						ImagePushedAt:  &oldTime,
					},
				},
				"other-repo": {
					{
						RegistryId:     aws.String("123456789"),
						RepositoryName: aws.String("other-repo"),
						ImageDigest:    aws.String("sha256:xyz789"),
						ImagePushedAt:  &oldTime,
					},
				},
			},
			cleanRepos:    []string{"test-repo"},
			expectedCount: 1,
		},
	}

	for _, g := range grid {
		t.Run(g.name, func(t *testing.T) {
			var images []types.ImageDetail
			var repositories []types.Repository
			var imageMap map[string][]types.ImageDetail
			var cleanRepos []string

			if g.images == nil {
				images = make([]types.ImageDetail, g.imageCount+g.newImageCount)
				for i := 0; i < g.imageCount; i++ {
					images[i] = types.ImageDetail{
						RegistryId:     aws.String("123456789"),
						RepositoryName: aws.String("test-repo"),
						ImageDigest:    aws.String(fmt.Sprintf("sha256:old%d", i)),
						ImagePushedAt:  &oldTime,
					}
				}
				for i := 0; i < g.newImageCount; i++ {
					images[g.imageCount+i] = types.ImageDetail{
						RegistryId:     aws.String("123456789"),
						RepositoryName: aws.String("test-repo"),
						ImageDigest:    aws.String(fmt.Sprintf("sha256:new%d", i)),
						ImagePushedAt:  &now,
					}
				}
				imageMap = map[string][]types.ImageDetail{
					"test-repo": images,
				}
			} else {
				imageMap = g.images
			}

			if g.repositories == nil {
				repositories = []types.Repository{
					{
						RegistryId:     aws.String("123456789"),
						RepositoryName: aws.String("test-repo"),
						RepositoryArn:  aws.String("arn:aws:ecr:us-west-2:123456789:repository/test-repo"),
					},
				}
			} else {
				repositories = g.repositories
			}

			if g.cleanRepos == nil {
				cleanRepos = []string{"test-repo"}
			} else {
				cleanRepos = g.cleanRepos
			}

			mock := &mockECRClient{
				repositories: repositories,
				images:       imageMap,
			}

			opts := Options{
				Account:              "123456789",
				Region:               "us-west-2",
				CleanEcrRepositories: cleanRepos,
				DryRun:               g.dryRun,
			}

			set := NewSet(24 * time.Hour)

			err := markAndSweepWithClient(mock, opts, set)
			if err != nil {
				t.Fatalf("markAndSweepWithClient failed: %v", err)
			}

			if mock.deletedCount != g.expectedCount {
				t.Errorf("expected %d images deleted, got %d", g.expectedCount, mock.deletedCount)
			}
		})
	}
}

func TestListAllWithClient(t *testing.T) {
	now := time.Now()

	grid := []struct {
		name          string
		repositories  []types.Repository
		images        map[string][]types.ImageDetail
		cleanRepos    []string
		expectedCount int
	}{
		{
			name: "lists all images in repository",
			repositories: []types.Repository{
				{
					RegistryId:     aws.String("123456789"),
					RepositoryName: aws.String("test-repo"),
				},
			},
			images: map[string][]types.ImageDetail{
				"test-repo": {
					{
						RegistryId:     aws.String("123456789"),
						RepositoryName: aws.String("test-repo"),
						ImageDigest:    aws.String("sha256:abc123"),
						ImagePushedAt:  &now,
					},
					{
						RegistryId:     aws.String("123456789"),
						RepositoryName: aws.String("test-repo"),
						ImageDigest:    aws.String("sha256:def456"),
						ImagePushedAt:  &now,
					},
				},
			},
			cleanRepos:    []string{"test-repo"},
			expectedCount: 2,
		},
		{
			name: "ignores non-configured repositories",
			repositories: []types.Repository{
				{
					RegistryId:     aws.String("123456789"),
					RepositoryName: aws.String("test-repo"),
				},
				{
					RegistryId:     aws.String("123456789"),
					RepositoryName: aws.String("other-repo"),
				},
			},
			images: map[string][]types.ImageDetail{
				"test-repo": {
					{
						RegistryId:     aws.String("123456789"),
						RepositoryName: aws.String("test-repo"),
						ImageDigest:    aws.String("sha256:abc123"),
						ImagePushedAt:  &now,
					},
				},
				"other-repo": {
					{
						RegistryId:     aws.String("123456789"),
						RepositoryName: aws.String("other-repo"),
						ImageDigest:    aws.String("sha256:xyz789"),
						ImagePushedAt:  &now,
					},
				},
			},
			cleanRepos:    []string{"test-repo"},
			expectedCount: 1,
		},
	}

	for _, g := range grid {
		t.Run(g.name, func(t *testing.T) {
			mock := &mockECRClient{
				repositories: g.repositories,
				images:       g.images,
			}

			opts := Options{
				Account:              "123456789",
				Region:               "us-west-2",
				CleanEcrRepositories: g.cleanRepos,
			}

			set := NewSet(0)

			result, err := listAllWithClient(mock, opts, set)
			if err != nil {
				t.Fatalf("listAllWithClient failed: %v", err)
			}

			if len(result.firstSeen) != g.expectedCount {
				t.Errorf("expected %d images in set, got %d", g.expectedCount, len(result.firstSeen))
			}
		})
	}
}
