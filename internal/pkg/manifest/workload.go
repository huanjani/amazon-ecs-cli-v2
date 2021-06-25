// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package manifest provides functionality to create Manifest files.
package manifest

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/copilot-cli/internal/pkg/template"
	"github.com/imdario/mergo"
	"gopkg.in/yaml.v3"
)

const (
	defaultSidecarPort = "80"

	defaultFluentbitImage = "amazon/aws-for-fluent-bit:latest"
)

var (
	errUnmarshalBuildOpts = errors.New("can't unmarshal build field into string or compose-style map")
	errUnmarshalCountOpts = errors.New(`unmarshal "count" field to an integer or autoscaling configuration`)
)

var dockerfileDefaultName = "Dockerfile"

// WorkloadProps contains properties for creating a new workload manifest.
type WorkloadProps struct {
	Name       string
	Dockerfile string
	Image      string
	//Platform   PlatformConfig
}

// Workload holds the basic data that every workload manifest file needs to have.
type Workload struct {
	Name *string `yaml:"name"`
	Type *string `yaml:"type"` // must be one of the supported manifest types.
}

// Image represents the workload's container image.
type Image struct {
	Build    BuildArgsOrString `yaml:"build"`    // Build an image from a Dockerfile.
	Location *string           `yaml:"location"` // Use an existing image instead.

	//Platform PlatformConfig    //`yaml:"platform"`
	//Platform     PlatformArgsOrString `yaml:"platform"`        // Include OS/Arch if host OS is Windows or Linux/ARM
	DockerLabels map[string]string `yaml:"labels,flow"`     // Apply Docker labels to the container at runtime.
	DependsOn    map[string]string `yaml:"depends_on,flow"` // Add any sidecar dependencies.
}

type workloadTransformer struct{}

// Transformer implements customized merge logic for Image field of manifest.
// It merges `DockerLabels` and `DependsOn` in the default manager (i.e. with configurations mergo.WithOverride, mergo.WithOverwriteWithEmptyValue)
// And then overrides both `Build` and `Location` fields at the same time with the src values, given that they are non-empty themselves.
func (t workloadTransformer) Transformer(typ reflect.Type) func(dst, src reflect.Value) error {
	if typ == reflect.TypeOf(Image{}) {
		return transformImage()
	}
	return nil
}

func transformImage() func(dst, src reflect.Value) error {
	return func(dst, src reflect.Value) error {
		// Perform default merge
		dstImage := dst.Interface().(Image)
		srcImage := src.Interface().(Image)

		err := mergo.Merge(&dstImage, srcImage, mergo.WithOverride, mergo.WithOverwriteWithEmptyValue)
		if err != nil {
			return err
		}

		// Perform customized merge
		dstBuild := dst.FieldByName("Build")
		dstLocation := dst.FieldByName("Location")

		srcBuild := src.FieldByName("Build")
		srcLocation := src.FieldByName("Location")

		if !srcBuild.IsZero() || !srcLocation.IsZero() {
			dstBuild.Set(srcBuild)
			dstLocation.Set(srcLocation)
		}
		return nil
	}
}

// ImageWithHealthcheck represents a container image with health check.
type ImageWithHealthcheck struct {
	Image       `yaml:",inline"`
	HealthCheck *ContainerHealthCheck `yaml:"healthcheck"`
}

// ImageWithPortAndHealthcheck represents a container image with an exposed port and health check.
type ImageWithPortAndHealthcheck struct {
	ImageWithPort `yaml:",inline"`
	HealthCheck   *ContainerHealthCheck `yaml:"healthcheck"`
}

// ImageWithPort represents a container image with an exposed port.
type ImageWithPort struct {
	Image `yaml:",inline"`
	Port  *uint16 `yaml:"port"`
}

// GetLocation returns the location of the image.
func (i Image) GetLocation() string {
	return aws.StringValue(i.Location)
}

// BuildConfig populates a docker.BuildArguments struct from the fields available in the manifest.
// Prefer the following hierarchy:
// 1. Specific dockerfile, specific context
// 2. Specific dockerfile, context = dockerfile dir
// 3. "Dockerfile" located in context dir
// 4. "Dockerfile" located in ws root.
func (i *Image) BuildConfig(rootDirectory string) *DockerBuildArgs {
	df := i.dockerfile()
	ctx := i.context()
	dockerfile := aws.String(filepath.Join(rootDirectory, dockerfileDefaultName))
	context := aws.String(rootDirectory)

	if df != "" && ctx != "" {
		dockerfile = aws.String(filepath.Join(rootDirectory, df))
		context = aws.String(filepath.Join(rootDirectory, ctx))
	}
	if df != "" && ctx == "" {
		dockerfile = aws.String(filepath.Join(rootDirectory, df))
		context = aws.String(filepath.Join(rootDirectory, filepath.Dir(df)))
	}
	if df == "" && ctx != "" {
		dockerfile = aws.String(filepath.Join(rootDirectory, ctx, dockerfileDefaultName))
		context = aws.String(filepath.Join(rootDirectory, ctx))
	}
	return &DockerBuildArgs{
		Dockerfile: dockerfile,
		Context:    context,
		Args:       i.args(),
		Target:     i.target(),
		CacheFrom:  i.cacheFrom(),
	}
}

// dockerfile returns the path to the workload's Dockerfile. If no dockerfile is specified,
// returns "".
func (i *Image) dockerfile() string {
	// Prefer to use the "Dockerfile" string in BuildArgs. Otherwise,
	// "BuildString". If no dockerfile specified, return "".
	if i.Build.BuildArgs.Dockerfile != nil {
		return aws.StringValue(i.Build.BuildArgs.Dockerfile)
	}

	var dfPath string
	if i.Build.BuildString != nil {
		dfPath = aws.StringValue(i.Build.BuildString)
	}

	return dfPath
}

// context returns the build context directory if it exists, otherwise an empty string.
func (i *Image) context() string {
	return aws.StringValue(i.Build.BuildArgs.Context)
}

// args returns the args section, if it exists, to override args in the dockerfile.
// Otherwise it returns an empty map.
func (i *Image) args() map[string]string {
	return i.Build.BuildArgs.Args
}

// target returns the build target stage if it exists, otherwise nil.
func (i *Image) target() *string {
	return i.Build.BuildArgs.Target
}

// cacheFrom returns the cache from build section, if it exists.
// Otherwise it returns nil.
func (i *Image) cacheFrom() []string {
	return i.Build.BuildArgs.CacheFrom
}

// BuildArgsOrString is a custom type which supports unmarshaling yaml which
// can either be of type string or type DockerBuildArgs.
type BuildArgsOrString struct {
	BuildString *string
	BuildArgs   DockerBuildArgs
}

func (b *BuildArgsOrString) isEmpty() bool {
	if aws.StringValue(b.BuildString) == "" && b.BuildArgs.isEmpty() {
		return true
	}
	return false
}

// UnmarshalYAML overrides the default YAML unmarshaling logic for the BuildArgsOrString
// struct, allowing it to perform more complex unmarshaling behavior.
// This method implements the yaml.Unmarshaler (v2) interface.
func (b *BuildArgsOrString) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if err := unmarshal(&b.BuildArgs); err != nil {
		switch err.(type) {
		case *yaml.TypeError:
			break
		default:
			return err
		}
	}

	if !b.BuildArgs.isEmpty() {
		// Unmarshaled successfully to b.BuildArgs, unset b.BuildString, and return.
		b.BuildString = nil
		return nil
	}

	if err := unmarshal(&b.BuildString); err != nil {
		return errUnmarshalBuildOpts
	}
	return nil
}

// DockerBuildArgs represents the options specifiable under the "build" field
// of Docker Compose services. For more information, see:
// https://docs.docker.com/compose/compose-file/#build
type DockerBuildArgs struct {
	Context    *string           `yaml:"context,omitempty"`
	Dockerfile *string           `yaml:"dockerfile,omitempty"`
	Args       map[string]string `yaml:"args,omitempty"`
	Target     *string           `yaml:"target,omitempty"`
	CacheFrom  []string          `yaml:"cache_from,omitempty"`
}

func (b *DockerBuildArgs) isEmpty() bool {
	if b.Context == nil && b.Dockerfile == nil && b.Args == nil && b.Target == nil && b.CacheFrom == nil {
		return true
	}
	return false
}

// PlatformArgsOrString is a custom type which supports unmarshaling yaml which
// can either be of type string or type PlatformArgs.
//type PlatformArgsOrString struct {
//	PlatformString *string
//	PlatformArgs   PlatformArgs
//}
//
//func (p *PlatformArgsOrString) isEmpty() bool {
//	if aws.StringValue(p.PlatformString) == "" && p.PlatformArgs.isEmpty() {
//		return true
//	}
//	return false
//}

// UnmarshalYAML overrides the default YAML unmarshaling logic for the PlatformArgsOrString
// struct, allowing it to perform more complex unmarshaling behavior.
// This method implements the yaml.Unmarshaler (v2) interface.
//func (p *PlatformArgsOrString) UnmarshalYAML(unmarshal func(interface{}) error) error {
//	if err := unmarshal(&p.PlatformArgs); err != nil {
//		switch err.(type) {
//		case *yaml.TypeError:
//			break
//		default:
//			return err
//		}
//	}
//
//	if !p.PlatformArgs.isEmpty() {
//		// Unmarshaled successfully to p.PlatformArgs, unset p.PlatformString, and return.
//		p.PlatformString = nil
//		return nil
//	}
//
//	if err := unmarshal(&p.PlatformString); err != nil {
//		return errUnmarshalBuildOpts
//	}
//	return nil
//}

// PlatformArgs represents the specifics of a target OS. For more information, see: TKTKTTK.
//type PlatformArgs struct {
//	OSFamily *string `yaml:"osfamily,omitempty"`
//	Arch     *string `yaml:"architecture,omitempty"`
//}
//
//func (p *PlatformArgs) isEmpty() bool {
//	if p.OSFamily == nil && p.Arch == nil {
//		return true
//	}
//	return false
//}

// ExecuteCommand is a custom type which supports unmarshaling yaml which
// can either be of type bool or type ExecuteCommandConfig.
type ExecuteCommand struct {
	Enable *bool
	Config ExecuteCommandConfig
}

// UnmarshalYAML overrides the default YAML unmarshaling logic for the BuildArgsOrString
// struct, allowing it to perform more complex unmarshaling behavior.
// This method implements the yaml.Unmarshaler (v2) interface.
func (e *ExecuteCommand) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if err := unmarshal(&e.Config); err != nil {
		switch err.(type) {
		case *yaml.TypeError:
			break
		default:
			return err
		}
	}

	if !e.Config.IsEmpty() {
		return nil
	}

	if err := unmarshal(&e.Enable); err != nil {
		return errUnmarshalExec
	}
	return nil
}

// ExecuteCommandConfig represents the configuration for ECS Execute Command.
type ExecuteCommandConfig struct {
	Enable *bool `yaml:"enable"`
	// Reserved for future use.
}

// IsEmpty returns whether ExecuteCommandConfig is empty.
func (e ExecuteCommandConfig) IsEmpty() bool {
	return e.Enable == nil
}

// Logging holds configuration for Firelens to route your logs.
type Logging struct {
	Image          *string           `yaml:"image"`
	Destination    map[string]string `yaml:"destination,flow"`
	EnableMetadata *bool             `yaml:"enableMetadata"`
	SecretOptions  map[string]string `yaml:"secretOptions"`
	ConfigFile     *string           `yaml:"configFilePath"`
}

func (lc *Logging) logConfigOpts() *template.LogConfigOpts {
	return &template.LogConfigOpts{
		Image:          lc.image(),
		ConfigFile:     lc.ConfigFile,
		EnableMetadata: lc.enableMetadata(),
		Destination:    lc.Destination,
		SecretOptions:  lc.SecretOptions,
	}
}

func (lc *Logging) image() *string {
	if lc.Image == nil {
		return aws.String(defaultFluentbitImage)
	}
	return lc.Image
}

func (lc *Logging) enableMetadata() *string {
	if lc.EnableMetadata == nil {
		// Enable ecs log metadata by default.
		return aws.String("true")
	}
	return aws.String(strconv.FormatBool(*lc.EnableMetadata))
}

// Sidecar holds configuration for all sidecar containers in a workload.
type Sidecar struct {
	Sidecars map[string]*SidecarConfig `yaml:"sidecars"`
}

// NetworkConfig represents options for network connection to AWS resources within a VPC.
type NetworkConfig struct {
	VPC *vpcConfig `yaml:"vpc"`
}

//PlatformConfig represents operating system and architecture variables.
//type PlatformConfig struct {
//	OS   string
//	Arch string
//}

// UnmarshalYAML ensures that a NetworkConfig always defaults to public subnets.
// If the user specified a placement that's not valid then throw an error.
func (c *NetworkConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type networkWithDefaults NetworkConfig
	defaultVPCConf := &vpcConfig{
		Placement: stringP(PublicSubnetPlacement),
	}
	conf := networkWithDefaults{
		VPC: defaultVPCConf,
	}
	if err := unmarshal(&conf); err != nil {
		return err
	}
	var sidecars []*template.SidecarOpts
	for name, config := range s.Sidecars {
		port, protocol, err := parsePortMapping(config.Port)
		if err != nil {
			return nil, err
		}
		sidecars = append(sidecars, &template.SidecarOpts{
			Name:       aws.String(name),
			Image:      config.Image,
			Port:       port,
			Protocol:   protocol,
			CredsParam: config.CredsParam,
		})
	}
	return sidecars, nil
}

// SidecarConfig represents the configurable options for setting up a sidecar container.
type SidecarConfig struct {
	Port       *string `yaml:"port"`
	Image      *string `yaml:"image"`
	CredsParam *string `yaml:"credentialsParameter"`
}

// Valid sidecar portMapping example: 2000/udp, or 2000 (default to be tcp).
func parsePortMapping(s *string) (port *string, protocol *string, err error) {
	if s == nil {
		// default port for sidecar container to be 80.
		return aws.String(defaultSidecarPort), nil, nil
	}
	portProtocol := strings.Split(*s, "/")
	switch len(portProtocol) {
	case 1:
		return aws.String(portProtocol[0]), nil, nil
	case 2:
		return aws.String(portProtocol[0]), aws.String(portProtocol[1]), nil
	default:
		return nil, nil, fmt.Errorf("cannot parse port mapping from %s", *s)
	}
}

// TaskConfig represents the resource boundaries and environment variables for the containers in the task.
type TaskConfig struct {
	CPU       *int              `yaml:"cpu"`
	Memory    *int              `yaml:"memory"`
	Count     Count             `yaml:"count"`
	Variables map[string]string `yaml:"variables"`
	Secrets   map[string]string `yaml:"secrets"`
}

// WorkloadProps contains properties for creating a new workload manifest.
type WorkloadProps struct {
	Name       string
	Dockerfile string
	Image      string
}

// UnmarshalWorkload deserializes the YAML input stream into a workload manifest object.
// If an error occurs during deserialization, then returns the error.
// If the workload type in the manifest is invalid, then returns an ErrInvalidManifestType.
func UnmarshalWorkload(in []byte) (interface{}, error) {
	am := Workload{}
	if err := yaml.Unmarshal(in, &am); err != nil {
		return nil, fmt.Errorf("unmarshal to workload manifest: %w", err)
	}
	typeVal := aws.StringValue(am.Type)

	switch typeVal {
	case LoadBalancedWebServiceType:
		m := newDefaultLoadBalancedWebService()
		if err := yaml.Unmarshal(in, m); err != nil {
			return nil, fmt.Errorf("unmarshal to load balanced web service: %w", err)
		}
		return m, nil
	case BackendServiceType:
		m := newDefaultBackendService()
		if err := yaml.Unmarshal(in, m); err != nil {
			return nil, fmt.Errorf("unmarshal to backend service: %w", err)
		}
		if m.BackendServiceConfig.ImageConfig.HealthCheck != nil {
			// Make sure that unset fields in the healthcheck gets a default value.
			m.BackendServiceConfig.ImageConfig.HealthCheck.applyIfNotSet(newDefaultContainerHealthCheck())
		}
		return m, nil
	case ScheduledJobType:
		m := newDefaultScheduledJob()
		if err := yaml.Unmarshal(in, m); err != nil {
			return nil, fmt.Errorf("unmarshal to scheduled job: %w", err)
		}
		return m, nil
	default:
		return nil, &ErrInvalidWorkloadType{Type: typeVal}
	}
}

func requiresBuild(image Image) (bool, error) {
	noBuild, noURL := image.Build.isEmpty(), image.Location == nil
	// Error if both of them are specified or neither is specified.
	if noBuild == noURL {
		return false, fmt.Errorf(`either "image.build" or "image.location" needs to be specified in the manifest`)
	}
	if image.Location == nil {
		return true, nil
	}
	return false, nil
}

func dockerfileBuildRequired(workloadType string, svc interface{}) (bool, error) {
	type manifest interface {
		BuildRequired() (bool, error)
	}
	mf, ok := svc.(manifest)
	if !ok {
		return false, fmt.Errorf("%s does not have required methods BuildRequired()", workloadType)
	}
	required, err := mf.BuildRequired()
	if err != nil {
		return false, fmt.Errorf("check if %s requires building from local Dockerfile: %w", workloadType, err)
	}
	return required, nil
}

func stringP(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func uint16P(n uint16) *uint16 {
	if n == 0 {
		return nil
	}
	return &n
}
