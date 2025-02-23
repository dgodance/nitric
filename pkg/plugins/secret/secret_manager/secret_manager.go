// Copyright 2021 Nitric Pty Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package secret_manager_secret_service

import (
	"context"
	"fmt"
	"strings"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	secretmanagerpb "google.golang.org/genproto/googleapis/cloud/secretmanager/v1"
	grpcCodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ifaces_gcloud_secret "github.com/nitrictech/nitric/pkg/ifaces/gcloud_secret"
	"github.com/nitrictech/nitric/pkg/plugins/errors"
	"github.com/nitrictech/nitric/pkg/plugins/errors/codes"
	"github.com/nitrictech/nitric/pkg/plugins/secret"
	"github.com/nitrictech/nitric/pkg/utils"
)

type secretManagerSecretService struct {
	secret.UnimplementedSecretPlugin
	client    ifaces_gcloud_secret.SecretManagerClient
	projectId string
	stackName string
	cache     map[string]string
}

func validateNewSecret(sec *secret.Secret, val []byte) error {
	if sec == nil {
		return fmt.Errorf("provide non-nil secret")
	}
	if len(sec.Name) == 0 {
		return fmt.Errorf("provide non-blank secret name")
	}
	if len(val) == 0 {
		return fmt.Errorf("provide non-blank secret value")
	}

	return nil
}

func (s *secretManagerSecretService) getParentName() string {
	return fmt.Sprintf("projects/%s", s.projectId)
}

func (s *secretManagerSecretService) buildSecretVersionName(sv *secret.SecretVersion) (string, error) {
	if len(sv.Secret.Name) == 0 {
		return "", fmt.Errorf("provide non-blank name")
	}

	if len(sv.Version) == 0 {
		return "", fmt.Errorf("provide non-blank version")
	}

	parent, inCache := s.cache[sv.Secret.Name]
	if !inCache {
		realSec, err := s.getSecret(sv.Secret)
		if err != nil {
			return "", err
		}

		parent = realSec.Name
	}

	return fmt.Sprintf("%s/versions/%s", parent, sv.Version), nil
}

// ensure a secret container exists for storing secret versions
func (s *secretManagerSecretService) getSecret(sec *secret.Secret) (*secretmanagerpb.Secret, error) {
	iter := s.client.ListSecrets(context.TODO(), &secretmanagerpb.ListSecretsRequest{
		Parent: s.getParentName(),
		Filter: "labels.x-nitric-name=" + sec.Name + " AND labels.x-nitric-stack=" + s.stackName,
	})

	result, err := iter.Next()
	if err == iterator.Done {
		return nil, status.Error(grpcCodes.NotFound, "secret not found")
	}

	if err != nil {
		return nil, err
	}

	s.cache[sec.Name] = result.Name

	return result, nil
}

// Put - Creates a new secret if one doesn't exist, or just adds a new secret version
func (s *secretManagerSecretService) Put(sec *secret.Secret, val []byte) (*secret.SecretPutResponse, error) {
	newErr := errors.ErrorsWithScope(
		"SecretManagerSecretService.Put",
		map[string]interface{}{
			"secret": sec,
		},
	)

	if err := validateNewSecret(sec, val); err != nil {
		return nil, newErr(
			codes.InvalidArgument,
			"invalid secret",
			err,
		)
	}

	// ensure the secret container exists...
	parentSec, err := s.getSecret(sec)
	if err != nil {
		return nil, newErr(
			codes.Internal,
			"error ensuring secret container exists",
			err,
		)
	}

	verResult, err := s.client.AddSecretVersion(context.TODO(), &secretmanagerpb.AddSecretVersionRequest{
		Parent: parentSec.Name,
		Payload: &secretmanagerpb.SecretPayload{
			Data: val,
		},
	})
	if err != nil {
		return nil, newErr(
			codes.Internal,
			"failed to add new secret version",
			err,
		)
	}

	versionStringParts := strings.Split(verResult.Name, "/")
	version := versionStringParts[len(versionStringParts)-1]

	return &secret.SecretPutResponse{
		SecretVersion: &secret.SecretVersion{
			Secret: &secret.Secret{
				Name: sec.Name,
			},
			Version: version,
		},
	}, nil
}

// Get - Retrieves a secret given a name and a version
func (s *secretManagerSecretService) Access(sv *secret.SecretVersion) (*secret.SecretAccessResponse, error) {
	newErr := errors.ErrorsWithScope(
		"SecretManagerSecretService.Access",
		map[string]interface{}{
			"version": sv,
		},
	)

	fullName, err := s.buildSecretVersionName(sv)
	if err != nil {
		return nil, newErr(
			codes.InvalidArgument,
			"invalid secret version",
			err,
		)
	}

	req := &secretmanagerpb.AccessSecretVersionRequest{
		Name: fullName,
	}

	result, err := s.client.AccessSecretVersion(context.TODO(), req)
	if err != nil {
		return nil, newErr(
			codes.Internal,
			"failed to access secret version",
			err,
		)
	}

	return &secret.SecretAccessResponse{
		// Return the original secret version payload
		SecretVersion: sv,
		Value:         result.Payload.GetData(),
	}, nil
}

// New - Creates a new Nitric secret service with GCP Secret Manager provider
func New() (secret.SecretService, error) {
	ctx := context.Background()

	credentials, credentialsError := google.FindDefaultCredentials(ctx, secretmanager.DefaultAuthScopes()...)
	if credentialsError != nil {
		return nil, fmt.Errorf("GCP credentials error: %v", credentialsError)
	}

	client, clientError := ifaces_gcloud_secret.NewClient(ctx)
	if clientError != nil {
		return nil, fmt.Errorf("secret manager client error: %v", clientError)
	}

	return &secretManagerSecretService{
		client:    client,
		projectId: credentials.ProjectID,
		stackName: utils.GetEnv("NITRIC_STACK", ""),
		cache:     make(map[string]string),
	}, nil
}
