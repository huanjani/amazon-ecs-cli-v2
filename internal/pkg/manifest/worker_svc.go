// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/copilot-cli/internal/pkg/template"
	"github.com/imdario/mergo"
)

const (
	workerSvcManifestPath = "workloads/services/worker/manifest.yml"
)

// WorkerService holds the configuration to create a worker service.
type WorkerService struct {
	Workload            `yaml:",inline"`
	WorkerServiceConfig `yaml:",inline"`
	// Use *WorkerServiceConfig because of https://github.com/imdario/mergo/issues/146
	Environments map[string]*WorkerServiceConfig `yaml:",flow"`

	parser template.Parser
}

// WorkerServiceConfig holds the configuration that can be overridden per environments.
type WorkerServiceConfig struct {
	ImageConfig   ImageWithHealthcheck `yaml:"image,flow"`
	ImageOverride `yaml:",inline"`
	TaskConfig    `yaml:",inline"`
	*Logging      `yaml:"logging,flow"`
	Sidecars      map[string]*SidecarConfig `yaml:"sidecars"`
	Subscribe     *SubscribeConfig          `yaml:"subscribe"`
	Network       *NetworkConfig            `yaml:"network"`
}

// WorkerServiceProps represents the configuration needed to create a worker service.
type WorkerServiceProps struct {
	WorkloadProps
	HealthCheck *ContainerHealthCheck // Optional healthcheck configuration.
	Topics      *[]TopicSubscription  // Optional topics for subscriptions
}

// SubscribeConfig represents the configurable options for setting up subscriptions.
type SubscribeConfig struct {
	Topics *[]TopicSubscription `yaml:"topics"`
}

// TopicSubscription represents the configurable options for setting up a SNS Topic Subscription.
type TopicSubscription struct {
	Name    string `yaml:"name"`
	Service string `yaml:"service"`
}

// NewWorkerService applies the props to a default Worker service configuration with
// minimal cpu/memory thresholds, single replica, no healthcheck, and then returns it.
func NewWorkerService(props WorkerServiceProps) *WorkerService {
	svc := newDefaultWorkerService()
	// Apply overrides.
	svc.Name = stringP(props.Name)
	svc.WorkerServiceConfig.ImageConfig.Image.Location = stringP(props.Image)
	svc.WorkerServiceConfig.ImageConfig.Build.BuildArgs.Dockerfile = stringP(props.Dockerfile)
	svc.WorkerServiceConfig.ImageConfig.HealthCheck = props.HealthCheck
	svc.WorkerServiceConfig.Subscribe.Topics = props.Topics
	svc.parser = template.New()
	return svc
}

// newDefaultWorkerService returns a Worker service with minimal task sizes and a single replica.
func newDefaultWorkerService() *WorkerService {
	return &WorkerService{
		Workload: Workload{
			Type: aws.String(WorkerServiceType),
		},
		WorkerServiceConfig: WorkerServiceConfig{
			ImageConfig: ImageWithHealthcheck{},
			Subscribe:   &SubscribeConfig{},
			TaskConfig: TaskConfig{
				CPU:    aws.Int(256),
				Memory: aws.Int(512),
				Count: Count{
					Value: aws.Int(1),
				},
				ExecuteCommand: ExecuteCommand{
					Enable: aws.Bool(false),
				},
			},
			Network: &NetworkConfig{
				VPC: &vpcConfig{
					Placement: aws.String(PublicSubnetPlacement),
				},
			},
		},
	}
}

// MarshalBinary serializes the manifest object into a binary YAML document.
// Implements the encoding.BinaryMarshaler interface.
func (s *WorkerService) MarshalBinary() ([]byte, error) {
	content, err := s.parser.Parse(workerSvcManifestPath, *s, template.WithFuncs(map[string]interface{}{
		"fmtSlice":   template.FmtSliceFunc,
		"quoteSlice": template.QuoteSliceFunc,
	}))
	if err != nil {
		return nil, err
	}
	return content.Bytes(), nil
}

// BuildRequired returns if the service requires building from the local Dockerfile.
func (s *WorkerService) BuildRequired() (bool, error) {
	return requiresBuild(s.ImageConfig.Image)
}

// TaskPlatform returns the os/arch for the service. This is an empty string if the default (linux/amd64) is detected.
func (s *WorkerService) TaskPlatform() string {
	return s.TaskConfig.Platform
}

// BuildArgs returns a docker.BuildArguments object for the service given a workspace root directory
func (s *WorkerService) BuildArgs(wsRoot string) *DockerBuildArgs {
	return s.ImageConfig.BuildConfig(wsRoot)
}

// ApplyEnv returns the service manifest with environment overrides.
// If the environment passed in does not have any overrides then it returns itself.
func (s WorkerService) ApplyEnv(envName string) (WorkloadManifest, error) {
	overrideConfig, ok := s.Environments[envName]
	if !ok {
		return &s, nil
	}

	if overrideConfig == nil {
		return &s, nil
	}

	envCount := overrideConfig.TaskConfig.Count
	if !envCount.IsEmpty() {
		s.TaskConfig.Count = envCount
	}

	// Apply overrides to the original service s.
	err := mergo.Merge(&s, WorkerService{
		WorkerServiceConfig: *overrideConfig,
	}, mergo.WithOverride, mergo.WithOverwriteWithEmptyValue, mergo.WithTransformers(workloadTransformer{}))

	if err != nil {
		return nil, err
	}
	s.Environments = nil
	return &s, nil
}
