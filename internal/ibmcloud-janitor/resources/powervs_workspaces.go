/*
Copyright 2025 The Kubernetes Authors.

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

	"github.com/IBM-Cloud/power-go-client/clients/instance"
	"github.com/IBM-Cloud/power-go-client/ibmpisession"
)

type PowerVSWorksapce struct {
	workspaceClient   *instance.IBMPIWorkspacesClient
	serviceInstanceID string
}

// Creates a new PowerVS workspace client
func NewWorkspaceClient(sess *ibmpisession.IBMPISession, instanceID string) *PowerVSWorksapce {
	c := &PowerVSWorksapce{
		serviceInstanceID: instanceID,
	}
	c.workspaceClient = instance.NewIBMPIWorkspacesClient(context.Background(), sess, instanceID)
	return c
}
