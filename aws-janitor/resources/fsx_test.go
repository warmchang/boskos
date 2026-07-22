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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/google/go-cmp/cmp"
)

type mockFSxPage struct {
	fileSystems []fsxtypes.FileSystem
	nextToken   *string
	err         error
}

type mockFSxClient struct {
	pages          map[string]mockFSxPage
	deleteErrors   map[string]error
	deleteInputs   []*fsx.DeleteFileSystemInput
	describeTokens []string
	events         []string
}

func (client *mockFSxClient) DescribeFileSystems(_ context.Context, input *fsx.DescribeFileSystemsInput, _ ...func(*fsx.Options)) (*fsx.DescribeFileSystemsOutput, error) {
	token := aws.ToString(input.NextToken)
	client.describeTokens = append(client.describeTokens, token)
	client.events = append(client.events, "describe:"+token)

	page, ok := client.pages[token]
	if !ok {
		return nil, fmt.Errorf("unexpected pagination token %q", token)
	}
	if page.err != nil {
		return nil, page.err
	}
	return &fsx.DescribeFileSystemsOutput{
		FileSystems: page.fileSystems,
		NextToken:   page.nextToken,
	}, nil
}

func (client *mockFSxClient) DeleteFileSystem(_ context.Context, input *fsx.DeleteFileSystemInput, _ ...func(*fsx.Options)) (*fsx.DeleteFileSystemOutput, error) {
	copiedInput := *input
	if input.LustreConfiguration != nil {
		copiedConfiguration := *input.LustreConfiguration
		copiedInput.LustreConfiguration = &copiedConfiguration
	}
	client.deleteInputs = append(client.deleteInputs, &copiedInput)

	id := aws.ToString(input.FileSystemId)
	client.events = append(client.events, "delete:"+id)
	if err := client.deleteErrors[id]; err != nil {
		return nil, err
	}
	return &fsx.DeleteFileSystemOutput{
		FileSystemId: input.FileSystemId,
		Lifecycle:    fsxtypes.FileSystemLifecycleDeleting,
	}, nil
}

func TestMarkAndSweepFSxLustreFileSystemsSelectsManagedResources(t *testing.T) {
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)
	ttlOverrideOld := now.Add(-2 * time.Hour)

	client := &mockFSxClient{
		pages: map[string]mockFSxPage{
			"": {
				fileSystems: []fsxtypes.FileSystem{
					testFSxFileSystem("fs-old", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, map[string]string{"owned": "true"}),
					testFSxFileSystem("fs-recent", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, recent, map[string]string{"owned": "true"}),
					testFSxFileSystem("fs-untagged", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, nil),
					testFSxFileSystem("fs-excluded", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, map[string]string{"owned": "true", "preserve": "true"}),
					testFSxFileSystem("fs-windows", fsxtypes.FileSystemTypeWindows, fsxtypes.FileSystemLifecycleAvailable, old, map[string]string{"owned": "true"}),
					testFSxFileSystem("fs-deleting", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleDeleting, old, map[string]string{"owned": "true"}),
					testFSxFileSystem("fs-ttl", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, ttlOverrideOld, map[string]string{"owned": "true", "janitor-ttl": "1h"}),
				},
			},
		},
	}

	opts := Options{
		Account:     "123456789012",
		Region:      "us-west-2",
		IncludeTags: mustTagMatcher(t, "owned=true"),
		ExcludeTags: mustTagMatcher(t, "preserve=true"),
		TTLTagKey:   "janitor-ttl",
	}
	if err := markAndSweepFSxLustreFileSystems(client, opts, NewSet(24*time.Hour)); err != nil {
		t.Fatalf("markAndSweepFSxLustreFileSystems() error = %v", err)
	}

	if diff := cmp.Diff([]string{"fs-old", "fs-ttl"}, deletedFSxIDs(client)); diff != "" {
		t.Errorf("deleted file systems mismatch (-want +got):\n%s", diff)
	}
	for _, input := range client.deleteInputs {
		if input.LustreConfiguration == nil || !aws.ToBool(input.LustreConfiguration.SkipFinalBackup) {
			t.Errorf("DeleteFileSystem(%q) did not explicitly skip the final backup", aws.ToString(input.FileSystemId))
		}
		if input.OpenZFSConfiguration != nil || input.WindowsConfiguration != nil {
			t.Errorf("DeleteFileSystem(%q) included a non-Lustre deletion configuration", aws.ToString(input.FileSystemId))
		}
	}
}

func TestMarkAndSweepFSxLustreFileSystemsDryRun(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	client := &mockFSxClient{
		pages: map[string]mockFSxPage{
			"": {
				fileSystems: []fsxtypes.FileSystem{
					testFSxFileSystem("fs-old", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, nil),
				},
			},
		},
	}

	if err := markAndSweepFSxLustreFileSystems(client, Options{DryRun: true}, NewSet(0)); err != nil {
		t.Fatalf("markAndSweepFSxLustreFileSystems() error = %v", err)
	}
	if len(client.deleteInputs) != 0 {
		t.Errorf("dry run issued %d DeleteFileSystem calls, want 0", len(client.deleteInputs))
	}
}

func TestMarkAndSweepFSxLustreFileSystemsPaginatesBeforeDeleting(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	client := &mockFSxClient{
		pages: map[string]mockFSxPage{
			"": {
				fileSystems: []fsxtypes.FileSystem{
					testFSxFileSystem("fs-first", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, nil),
				},
				nextToken: aws.String("second"),
			},
			"second": {
				fileSystems: []fsxtypes.FileSystem{
					testFSxFileSystem("fs-second", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, nil),
				},
			},
		},
	}

	if err := markAndSweepFSxLustreFileSystems(client, Options{}, NewSet(0)); err != nil {
		t.Fatalf("markAndSweepFSxLustreFileSystems() error = %v", err)
	}
	wantEvents := []string{"describe:", "describe:second", "delete:fs-first", "delete:fs-second"}
	if diff := cmp.Diff(wantEvents, client.events); diff != "" {
		t.Errorf("API call order mismatch (-want +got):\n%s", diff)
	}
}

func TestMarkAndSweepFSxLustreFileSystemsDoesNotDeleteAfterDescribeFailure(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	describeErr := errors.New("describe failed")
	client := &mockFSxClient{
		pages: map[string]mockFSxPage{
			"": {
				fileSystems: []fsxtypes.FileSystem{
					testFSxFileSystem("fs-first", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, nil),
				},
				nextToken: aws.String("second"),
			},
			"second": {err: describeErr},
		},
	}

	err := markAndSweepFSxLustreFileSystems(client, Options{}, NewSet(0))
	if !errors.Is(err, describeErr) {
		t.Fatalf("markAndSweepFSxLustreFileSystems() error = %v, want wrapped describe error", err)
	}
	if len(client.deleteInputs) != 0 {
		t.Errorf("issued %d DeleteFileSystem calls after pagination failed, want 0", len(client.deleteInputs))
	}
}

func TestMarkAndSweepFSxLustreFileSystemsContinuesAfterDeleteFailure(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	client := &mockFSxClient{
		pages: map[string]mockFSxPage{
			"": {
				fileSystems: []fsxtypes.FileSystem{
					testFSxFileSystem("fs-first", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, nil),
					testFSxFileSystem("fs-second", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, nil),
				},
			},
		},
		deleteErrors: map[string]error{"fs-first": errors.New("delete failed")},
	}

	if err := markAndSweepFSxLustreFileSystems(client, Options{}, NewSet(0)); err != nil {
		t.Fatalf("markAndSweepFSxLustreFileSystems() error = %v", err)
	}
	if diff := cmp.Diff([]string{"fs-first", "fs-second"}, deletedFSxIDs(client)); diff != "" {
		t.Errorf("delete attempts mismatch (-want +got):\n%s", diff)
	}
}

func TestMarkAndSweepFSxLustreFileSystemsRejectsIncompleteMetadata(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	testCases := []struct {
		name   string
		mutate func(*fsxtypes.FileSystem)
	}{
		{
			name: "missing ID",
			mutate: func(fileSystem *fsxtypes.FileSystem) {
				fileSystem.FileSystemId = nil
			},
		},
		{
			name: "missing ARN",
			mutate: func(fileSystem *fsxtypes.FileSystem) {
				fileSystem.ResourceARN = nil
			},
		},
		{
			name: "missing tag key",
			mutate: func(fileSystem *fsxtypes.FileSystem) {
				fileSystem.Tags = []fsxtypes.Tag{{Value: aws.String("value")}}
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			valid := testFSxFileSystem("fs-valid", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, nil)
			invalid := testFSxFileSystem("fs-invalid", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, old, nil)
			testCase.mutate(&invalid)
			client := &mockFSxClient{
				pages: map[string]mockFSxPage{
					"": {fileSystems: []fsxtypes.FileSystem{valid, invalid}},
				},
			}

			if err := markAndSweepFSxLustreFileSystems(client, Options{}, NewSet(0)); err == nil {
				t.Fatal("markAndSweepFSxLustreFileSystems() error = nil, want metadata validation error")
			}
			if len(client.deleteInputs) != 0 {
				t.Errorf("issued %d DeleteFileSystem calls with incomplete metadata, want 0", len(client.deleteInputs))
			}
		})
	}
}

func TestListAllFSxLustreFileSystems(t *testing.T) {
	now := time.Now()
	client := &mockFSxClient{
		pages: map[string]mockFSxPage{
			"": {
				fileSystems: []fsxtypes.FileSystem{
					testFSxFileSystem("fs-lustre-a", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleAvailable, now, nil),
					testFSxFileSystem("fs-windows", fsxtypes.FileSystemTypeWindows, fsxtypes.FileSystemLifecycleAvailable, now, nil),
				},
				nextToken: aws.String("second"),
			},
			"second": {
				fileSystems: []fsxtypes.FileSystem{
					testFSxFileSystem("fs-lustre-b", fsxtypes.FileSystemTypeLustre, fsxtypes.FileSystemLifecycleCreating, now, nil),
				},
			},
		},
	}

	set, err := listAllFSxLustreFileSystems(client, Options{Account: "123456789012", Region: "us-west-2"})
	if err != nil {
		t.Fatalf("listAllFSxLustreFileSystems() error = %v", err)
	}
	wantARNs := []string{
		testFSxARN("fs-lustre-a"),
		testFSxARN("fs-lustre-b"),
	}
	if diff := cmp.Diff(wantARNs, set.GetARNs()); diff != "" {
		t.Errorf("listed file systems mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"", "second"}, client.describeTokens); diff != "" {
		t.Errorf("pagination tokens mismatch (-want +got):\n%s", diff)
	}
}

func TestListAllFSxLustreFileSystemsReturnsDescribeError(t *testing.T) {
	describeErr := errors.New("describe failed")
	client := &mockFSxClient{
		pages: map[string]mockFSxPage{"": {err: describeErr}},
	}

	_, err := listAllFSxLustreFileSystems(client, Options{})
	if !errors.Is(err, describeErr) {
		t.Fatalf("listAllFSxLustreFileSystems() error = %v, want wrapped describe error", err)
	}
}

func TestFSxLustreFileSystemsRegisteredBeforeNetworkResources(t *testing.T) {
	fsxIndex := -1
	networkInterfacesIndex := -1
	for i, resourceType := range RegionalTypeList {
		switch resourceType.(type) {
		case FSxLustreFileSystems:
			fsxIndex = i
		case NetworkInterfaces:
			networkInterfacesIndex = i
		}
	}

	if fsxIndex == -1 {
		t.Fatal("FSxLustreFileSystems is not registered in RegionalTypeList")
	}
	if networkInterfacesIndex == -1 {
		t.Fatal("NetworkInterfaces is not registered in RegionalTypeList")
	}
	if fsxIndex >= networkInterfacesIndex {
		t.Errorf("FSxLustreFileSystems index = %d, want before NetworkInterfaces index %d", fsxIndex, networkInterfacesIndex)
	}
}

func testFSxFileSystem(id string, fileSystemType fsxtypes.FileSystemType, lifecycle fsxtypes.FileSystemLifecycle, creationTime time.Time, tagValues map[string]string) fsxtypes.FileSystem {
	tags := make([]fsxtypes.Tag, 0, len(tagValues))
	for key, value := range tagValues {
		tags = append(tags, fsxtypes.Tag{Key: aws.String(key), Value: aws.String(value)})
	}
	return fsxtypes.FileSystem{
		CreationTime:   &creationTime,
		FileSystemId:   aws.String(id),
		FileSystemType: fileSystemType,
		Lifecycle:      lifecycle,
		ResourceARN:    aws.String(testFSxARN(id)),
		Tags:           tags,
	}
}

func testFSxARN(id string) string {
	return "arn:aws:fsx:us-west-2:123456789012:file-system/" + id
}

func mustTagMatcher(t *testing.T, tags ...string) TagMatcher {
	t.Helper()
	matcher, err := TagMatcherForTags(tags)
	if err != nil {
		t.Fatalf("TagMatcherForTags(%v) error = %v", tags, err)
	}
	return matcher
}

func deletedFSxIDs(client *mockFSxClient) []string {
	ids := make([]string, 0, len(client.deleteInputs))
	for _, input := range client.deleteInputs {
		ids = append(ids, aws.ToString(input.FileSystemId))
	}
	return ids
}
