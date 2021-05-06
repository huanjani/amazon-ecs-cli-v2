// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/copilot-cli/internal/pkg/template"
	"github.com/imdario/mergo"
)

const (
	backendSvcManifestPath = "workloads/services/backend/manifest.yml"
)

// BackendServiceProps represents the configuration needed to create a backend service.
type BackendServiceProps struct {
	WorkloadProps
	Port        uint16
	HealthCheck *ContainerHealthCheck // Optional healthcheck configuration.
}

// BackendService holds the configuration to create a backend service manifest.
type BackendService struct {
	Workload             `yaml:",inline"`
	BackendServiceConfig `yaml:",inline"`
	// Use *BackendServiceConfig because of https://github.com/imdario/mergo/issues/146
	Environments map[string]*BackendServiceConfig `yaml:",flow"`

	parser template.Parser
}

// BackendServiceConfig holds the configuration that can be overriden per environments.
type BackendServiceConfig struct {
	ImageConfig   imageWithPortAndHealthcheck `yaml:"image,flow"`
	ImageOverride `yaml:",inline"`
	TaskConfig    `yaml:",inline"`
	*Logging      `yaml:"logging,flow"`
	Sidecars      map[string]*SidecarConfig `yaml:"sidecars"`
	Network       NetworkConfig             `yaml:"network"`
}

type imageWithPortAndHealthcheck struct {
	ServiceImageWithPort `yaml:",inline"`
	HealthCheck          *ContainerHealthCheck `yaml:"healthcheck"`
}

// ContainerHealthCheck holds the configuration to determine if the service container is healthy.
// See https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/aws-properties-ecs-taskdefinition-healthcheck.html
type ContainerHealthCheck struct {
	Command     []string       `yaml:"command"`
	Interval    *time.Duration `yaml:"interval"`
	Retries     *int           `yaml:"retries"`
	Timeout     *time.Duration `yaml:"timeout"`
	StartPeriod *time.Duration `yaml:"start_period"`
}

// NewBackendService applies the props to a default backend service configuration with
// minimal task sizes, single replica, no healthcheck, and then returns it.
func NewBackendService(props BackendServiceProps) *BackendService {
	svc := newDefaultBackendService()
	var healthCheck *ContainerHealthCheck
	if props.HealthCheck != nil {
		// Create the healthcheck field only if the caller specified a healthcheck.
		healthCheck = newDefaultContainerHealthCheck()
		healthCheck.apply(props.HealthCheck)
	}
	// Apply overrides.
	svc.Name = aws.String(props.Name)
	svc.BackendServiceConfig.ImageConfig.Image.Location = stringP(props.Image)
	svc.BackendServiceConfig.ImageConfig.Build.BuildArgs.Dockerfile = stringP(props.Dockerfile)
	svc.BackendServiceConfig.ImageConfig.Port = uint16P(props.Port)
	svc.BackendServiceConfig.ImageConfig.HealthCheck = healthCheck
	svc.parser = template.New()
	return svc
}

// MarshalBinary serializes the manifest object into a binary YAML document.
// Implements the encoding.BinaryMarshaler interface.
func (s *BackendService) MarshalBinary() ([]byte, error) {
	content, err := s.parser.Parse(backendSvcManifestPath, *s, template.WithFuncs(map[string]interface{}{
		"fmtSlice":   template.FmtSliceFunc,
		"quoteSlice": template.QuoteSliceFunc,
		"dirName":    tplDirName,
	}))
	if err != nil {
		return nil, err
	}
	return content.Bytes(), nil
}

// BuildRequired returns if the service requires building from the local Dockerfile.
func (s *BackendService) BuildRequired() (bool, error) {
	return requiresBuild(s.ImageConfig.Image)
}

// BuildArgs returns a docker.BuildArguments object for the service given a workspace root directory
func (s *BackendService) BuildArgs(wsRoot string) *DockerBuildArgs {
	return s.ImageConfig.BuildConfig(wsRoot)
}

// ApplyEnv returns the service manifest with environment overrides.
// If the environment passed in does not have any overrides then it returns itself.
func (s BackendService) ApplyEnv(envName string) (*BackendService, error) {
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
	err := mergo.Merge(&s, BackendService{
		BackendServiceConfig: *overrideConfig,
	}, mergo.WithOverride, mergo.WithOverwriteWithEmptyValue)
	if err != nil {
		return nil, err
	}
	s.Environments = nil
	return &s, nil
}

// newDefaultBackendService returns a backend service with minimal task sizes and a single replica.
func newDefaultBackendService() *BackendService {
	return &BackendService{
		Workload: Workload{
			Type: aws.String(BackendServiceType),
		},
		BackendServiceConfig: BackendServiceConfig{
			ImageConfig: imageWithPortAndHealthcheck{},
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
			Network: NetworkConfig{
				VPC: vpcConfig{
					Placement: stringP(PublicSubnetPlacement),
				},
			},
		},
	}
}

// newDefaultContainerHealthCheck returns container health check configuration
// that's identical to a load balanced web service's defaults.
func newDefaultContainerHealthCheck() *ContainerHealthCheck {
	return &ContainerHealthCheck{
		Command:     []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"},
		Interval:    durationp(10 * time.Second),
		Retries:     aws.Int(2),
		Timeout:     durationp(5 * time.Second),
		StartPeriod: durationp(0 * time.Second),
	}
}

// apply overrides the healthcheck's fields if other has them set.
func (hc *ContainerHealthCheck) apply(other *ContainerHealthCheck) {
	if other.Command != nil {
		hc.Command = other.Command
	}
	if other.Interval != nil {
		hc.Interval = other.Interval
	}
	if other.Retries != nil {
		hc.Retries = other.Retries
	}
	if other.Timeout != nil {
		hc.Timeout = other.Timeout
	}
	if other.StartPeriod != nil {
		hc.StartPeriod = other.StartPeriod
	}
}

// applyIfNotSet changes the healthcheck's fields only if they were not set and the other healthcheck has them set.
func (hc *ContainerHealthCheck) applyIfNotSet(other *ContainerHealthCheck) {
	if hc.Command == nil && other.Command != nil {
		hc.Command = other.Command
	}
	if hc.Interval == nil && other.Interval != nil {
		hc.Interval = other.Interval
	}
	if hc.Retries == nil && other.Retries != nil {
		hc.Retries = other.Retries
	}
	if hc.Timeout == nil && other.Timeout != nil {
		hc.Timeout = other.Timeout
	}
	if hc.StartPeriod == nil && other.StartPeriod != nil {
		hc.StartPeriod = other.StartPeriod
	}
}

// HealthCheckOpts converts the image's healthcheck configuration into a format parsable by the templates pkg.
func (i imageWithPortAndHealthcheck) HealthCheckOpts() *ecs.HealthCheck {
	if i.HealthCheck == nil {
		return nil
	}
	return &ecs.HealthCheck{
		Command:     aws.StringSlice(i.HealthCheck.Command),
		Interval:    aws.Int64(int64(i.HealthCheck.Interval.Seconds())),
		Retries:     aws.Int64(int64(*i.HealthCheck.Retries)),
		StartPeriod: aws.Int64(int64(i.HealthCheck.StartPeriod.Seconds())),
		Timeout:     aws.Int64(int64(i.HealthCheck.Timeout.Seconds())),
	}
}
