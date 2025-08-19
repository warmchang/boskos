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
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"

	"github.com/sirupsen/logrus"
)

type ecrClient interface {
	DescribeRepositories(ctx context.Context, params *ecr.DescribeRepositoriesInput, optFns ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error)
	DescribeImages(ctx context.Context, params *ecr.DescribeImagesInput, optFns ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error)
	BatchDeleteImage(ctx context.Context, params *ecr.BatchDeleteImageInput, optFns ...func(*ecr.Options)) (*ecr.BatchDeleteImageOutput, error)
}

type ContainerImages struct{}

func (ContainerImages) MarkAndSweep(opts Options, set *Set) error {
	logger := logrus.WithField("options", opts)
	if len(opts.CleanEcrRepositories) == 0 {
		logger.Info("No ECR reposotories to clean provided, skipping ECR")
		return nil
	}

	svc := ecr.NewFromConfig(*opts.Config, func(opt *ecr.Options) {
		opt.Region = opts.Region
	})
	return markAndSweepWithClient(svc, opts, set)
}

//nolint:gocognit // Just over the complexity threshold and splitting would make it less readable
func markAndSweepWithClient(svc ecrClient, opts Options, set *Set) error {
	logger := logrus.WithField("options", opts)
	inp := &ecr.DescribeRepositoriesInput{
		RegistryId: aws.String(opts.Account),
	}

	// DescribeImages requires a repository name, so we first must crawl all repositories
	err := DescribeRepositoriesPages(svc, inp, func(repos *ecr.DescribeRepositoriesOutput) error {
		for _, repo := range repos.Repositories {
			if !slices.Contains(opts.CleanEcrRepositories, *repo.RepositoryName) {
				continue
			}

			// BatchDeleteImage can only be called per-repo, so describe all the images in a repo, delete them, and repeat
			var toDelete []*image
			imageInp := &ecr.DescribeImagesInput{
				RegistryId:     repo.RegistryId,
				RepositoryName: repo.RepositoryName,
			}

			imagesErr := DescribeImagesPages(svc, imageInp, func(images *ecr.DescribeImagesOutput) error {
				for _, ecrImage := range images.ImageDetails {
					i := image{
						Registry:   *ecrImage.RegistryId,
						Region:     opts.Region,
						Repository: *ecrImage.RepositoryName,
						Digest:     *ecrImage.ImageDigest,
					}

					// ECR repos cannot have tags, so pass an empty object
					if !set.Mark(opts, i, ecrImage.ImagePushedAt, Tags{}) {
						continue
					}
					toDelete = append(toDelete, &i)
				}
				return nil
			}, false /* continue on error */)
			if imagesErr != nil {
				logrus.Warningf("failed to get page: %v", imagesErr)
			}

			deleteReq := &ecr.BatchDeleteImageInput{
				RegistryId:     repo.RegistryId,
				RepositoryName: repo.RepositoryName,
				ImageIds:       []types.ImageIdentifier{},
			}
			for n, image := range toDelete {
				deleteReq.ImageIds = append(deleteReq.ImageIds, types.ImageIdentifier{
					ImageDigest: aws.String(image.Digest),
				})

				// 100 images max: https://docs.aws.amazon.com/AmazonECR/latest/APIReference/API_BatchDeleteImage.html
				// If we've reached 100 images, or this is the last item in the toDelete slice, make the call to the AWS API
				if len(deleteReq.ImageIds) == 100 || n == len(toDelete)-1 {
					logger.Warningf("%s: deleting %d images", *repo.RepositoryArn, len(deleteReq.ImageIds))
					if !opts.DryRun {
						result, err := svc.BatchDeleteImage(context.TODO(), deleteReq)

						if err != nil {
							logger.Warningf("%s: delete API call failed: %v", *repo.RepositoryArn, err)
						} else if len(result.Failures) > 0 {
							logger.Warningf("%s: some images failed to delete: %T", *repo.RepositoryArn, result.Failures)
						}
					}
					// After making delete call, reset to an empty set of images
					deleteReq.ImageIds = []types.ImageIdentifier{}
				}
			}
		}
		return nil
	}, false /* continue on error */)
	if err != nil {
		logrus.Warningf("failed to get page: %v", err)
	}

	return nil
}

func (ContainerImages) ListAll(opts Options) (*Set, error) {
	set := NewSet(0)
	if len(opts.CleanEcrRepositories) == 0 {
		return set, nil
	}
	svc := ecr.NewFromConfig(*opts.Config, func(opt *ecr.Options) {
		opt.Region = opts.Region
	})
	return listAllWithClient(svc, opts, set)
}

func listAllWithClient(svc ecrClient, opts Options, set *Set) (*Set, error) {
	inp := &ecr.DescribeRepositoriesInput{
		RegistryId: aws.String(opts.Account),
	}

	err := DescribeRepositoriesPages(svc, inp, func(repos *ecr.DescribeRepositoriesOutput) error {
		for _, repo := range repos.Repositories {
			if !slices.Contains(opts.CleanEcrRepositories, *repo.RepositoryName) {
				continue
			}

			imageInp := &ecr.DescribeImagesInput{
				RegistryId:     repo.RegistryId,
				RepositoryName: repo.RepositoryName,
			}

			err := DescribeImagesPages(svc, imageInp, func(images *ecr.DescribeImagesOutput) error {
				now := time.Now()
				for _, ecrImage := range images.ImageDetails {
					key := image{
						Registry:   *ecrImage.RegistryId,
						Region:     opts.Region,
						Repository: *ecrImage.RepositoryName,
						Digest:     *ecrImage.ImageDigest,
					}.ResourceKey()

					set.firstSeen[key] = now
				}
				return nil
			}, true /* fail on error */)

			if err != nil {
				return err
			}
		}
		return nil
	}, true /* fail on error */)

	return set, err
}

//nolint:nestif // Ifs are small and nesting cannot be reasonably avoided
func DescribeRepositoriesPages(svc ecrClient, input *ecr.DescribeRepositoriesInput, pageFunc func(repos *ecr.DescribeRepositoriesOutput) error, failOnError bool) error {
	paginator := ecr.NewDescribeRepositoriesPaginator(svc, input)

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.TODO())
		if err != nil {
			logrus.Warningf("failed to get page: %v", err)
			if failOnError {
				return err
			}
		} else {
			err = pageFunc(page)
			if err != nil {
				logrus.Warningf("pageFunc failed: %v", err)
				if failOnError {
					return err
				}
			}
		}
	}
	return nil
}

//nolint:nestif // Ifs are small and nesting cannot be reasonably avoided
func DescribeImagesPages(svc ecrClient, input *ecr.DescribeImagesInput, pageFunc func(images *ecr.DescribeImagesOutput) error, failOnError bool) error {
	paginator := ecr.NewDescribeImagesPaginator(svc, input)

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.TODO())
		if err != nil {
			logrus.Warningf("failed to get page: %v", err)
			if failOnError {
				return err
			}
		} else {
			err = pageFunc(page)
			if err != nil {
				logrus.Warningf("pageFunc failed: %v", err)
				if failOnError {
					return err
				}
			}
		}
	}
	return nil
}

type image struct {
	Registry   string
	Region     string
	Repository string
	Digest     string
}

// Images don't have an ARN, only repositories do, so we use our own custom key format
func (i image) ARN() string {
	return i.ResourceKey()
}

func (i image) ResourceKey() string {
	return fmt.Sprintf("%s:%s/%s:%s", i.Registry, i.Region, i.Repository, i.Digest)
}
