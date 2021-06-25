// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/copilot-cli/internal/pkg/aws/sessions"
	"github.com/aws/copilot-cli/internal/pkg/config"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation"
	"github.com/aws/copilot-cli/internal/pkg/docker/dockerfile"
	"github.com/aws/copilot-cli/internal/pkg/initialize"
	"github.com/aws/copilot-cli/internal/pkg/manifest"
	"github.com/aws/copilot-cli/internal/pkg/term/color"
	"github.com/aws/copilot-cli/internal/pkg/term/log"
	termprogress "github.com/aws/copilot-cli/internal/pkg/term/progress"
	"github.com/aws/copilot-cli/internal/pkg/term/prompt"
	"github.com/aws/copilot-cli/internal/pkg/term/selector"
	"github.com/aws/copilot-cli/internal/pkg/workspace"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	fmtSvcInitSvcTypePrompt     = "Which %s best represents your service's architecture?"
	fmtSvcInitSvcTypeHelpPrompt = `A %s is a public, internet-facing, HTTP server that's behind a load balancer. 
To learn more see: https://git.io/JfIpv

A %s is a private, non internet-facing service.
To learn more see: https://git.io/JfIpT`

	fmtWkldInitNamePrompt     = "What do you want to %s this %s?"
	fmtWkldInitNameHelpPrompt = `The name will uniquely identify this %s within your app %s.
Deployed resources (such as your ECR repository, logs) will contain this %[1]s's name and be tagged with it.`

	fmtWkldInitDockerfilePrompt      = "Which " + color.Emphasize("Dockerfile") + " would you like to use for %s?"
	wkldInitDockerfileHelpPrompt     = "Dockerfile to use for building your container image."
	fmtWkldInitDockerfilePathPrompt  = "What is the path to the " + color.Emphasize("Dockerfile") + " for %s?"
	wkldInitDockerfilePathHelpPrompt = "Path to Dockerfile to use for building your container image."

	svcInitSvcPortPrompt     = "Which %s do you want customer traffic sent to?"
	svcInitSvcPortHelpPrompt = `The port will be used by the load balancer to route incoming traffic to this service.
You should set this to the port which your Dockerfile uses to communicate with the internet.`
)

const (
	wkldInitImagePrompt     = `What's the location of the image to use?`
	wkldInitImagePromptHelp = `The name of an existing Docker image. Images in the Docker Hub registry are available by default.
Other repositories are specified with either repository-url/image:tag or repository-url/image@digest`
)

const (
	defaultSvcPortString = "80"
	service              = "service"
)

type initWkldVars struct {
	appName        string
	wkldType       string
	name           string
	dockerfilePath string
	image          string
}

type initSvcVars struct {
	initWkldVars

	port uint16
}

type initSvcOpts struct {
	initSvcVars

	// Interfaces to interact with dependencies.
	fs     afero.Fs
	init   svcInitializer
	prompt prompter
	df     dockerfileParser

	sel dockerfileSelector

	// Outputs stored on successful actions.
	manifestPath string
	//platform     string

	// Sets up Dockerfile parser using fs and input path
	setupParser func(*initSvcOpts)
}

func newInitSvcOpts(vars initSvcVars) (*initSvcOpts, error) {
	store, err := config.NewStore()
	if err != nil {
		return nil, fmt.Errorf("couldn't connect to config store: %w", err)
	}

	ws, err := workspace.New()
	if err != nil {
		return nil, fmt.Errorf("workspace cannot be created: %w", err)
	}

	p := sessions.NewProvider()
	sess, err := p.Default()
	if err != nil {
		return nil, err
	}
	prompter := prompt.New()
	sel := selector.NewWorkspaceSelect(prompter, store, ws)

	initSvc := &initialize.WorkloadInitializer{
		Store:    store,
		Ws:       ws,
		Prog:     termprogress.NewSpinner(),
		Deployer: cloudformation.New(sess),
	}
	return &initSvcOpts{
		initSvcVars: vars,

		fs:     &afero.Afero{Fs: afero.NewOsFs()},
		init:   initSvc,
		prompt: prompter,
		sel:    sel,

		setupParser: func(o *initSvcOpts) {
			o.df = dockerfile.New(o.fs, o.dockerfilePath)
		},
	}, nil
}

// Validate returns an error if the flag values passed by the user are invalid.
func (o *initSvcOpts) Validate() error {
	if o.appName == "" {
		return errNoAppInWorkspace
	}
	if o.wkldType != "" {
		if err := validateSvcType(o.wkldType); err != nil {
			return err
		}
	}
	if o.name != "" {
		if err := validateSvcName(o.name); err != nil {
			return err
		}
	}
	if o.dockerfilePath != "" && o.image != "" {
		return fmt.Errorf("--%s and --%s cannot be specified together", dockerFileFlag, imageFlag)
	}
	if o.dockerfilePath != "" {
		if _, err := o.fs.Stat(o.dockerfilePath); err != nil {
			return err
		}
	}
	if o.port != 0 {
		if err := validateSvcPort(o.port); err != nil {
			return err
		}
	}
	return nil
}

// Ask prompts for fields that are required but not passed in.
func (o *initSvcOpts) Ask() error {
	if err := o.askSvcType(); err != nil {
		return err
	}
	if err := o.askSvcName(); err != nil {
		return err
	}
	dfSelected, err := o.askDockerfile()
	if err != nil {
		return err
	}

	if !dfSelected {
		if err := o.askImage(); err != nil {
			return err
		}
	}

	if err := o.askSvcPort(); err != nil {
		return err
	}

	return nil
}

// Execute writes the service's manifest file and stores the service in SSM.
func (o *initSvcOpts) Execute() error {
	// Check for a valid healthcheck and add it to the opts.
	var hc *manifest.ContainerHealthCheck
	hc, err := o.parseHealthCheck()
	if err != nil {
		return err
	}

	manifestPath, err := o.init.Service(&initialize.ServiceProps{
		WorkloadProps: initialize.WorkloadProps{
			App:            o.appName,
			Name:           o.name,
			Type:           o.wkldType,
			DockerfilePath: o.dockerfilePath,
			Image:          o.image,
			//Platform:       o.platform,
		},
		Port:        o.port,
		HealthCheck: hc,
	})
	if err != nil {
		return err
	}
	o.manifestPath = manifestPath
	return nil
}

func (o *initSvcOpts) askSvcType() error {
	if o.wkldType != "" {
		return nil
	}

	help := fmt.Sprintf(fmtSvcInitSvcTypeHelpPrompt,
		manifest.LoadBalancedWebServiceType,
		manifest.BackendServiceType,
	)
	msg := fmt.Sprintf(fmtSvcInitSvcTypePrompt, color.Emphasize("service type"))
	t, err := o.prompt.SelectOne(msg, help, manifest.ServiceTypes, prompt.WithFinalMessage("Service type:"))
	if err != nil {
		return fmt.Errorf("select service type: %w", err)
	}
	o.wkldType = t
	return nil
}

func (o *initSvcOpts) askSvcName() error {
	if o.name != "" {
		return nil
	}

	name, err := o.prompt.Get(
		fmt.Sprintf(fmtWkldInitNamePrompt, color.Emphasize("name"), color.HighlightUserInput(o.wkldType)),
		fmt.Sprintf(fmtWkldInitNameHelpPrompt, service, o.appName),
		validateSvcName,
		prompt.WithFinalMessage("Service name:"))
	if err != nil {
		return fmt.Errorf("get service name: %w", err)
	}
	o.name = name
	return nil
}

func (o *initSvcOpts) askImage() error {
	if o.image != "" {
		return nil
	}
	image, err := o.prompt.Get(wkldInitImagePrompt, wkldInitImagePromptHelp, nil,
		prompt.WithFinalMessage("Image:"))
	if err != nil {
		return fmt.Errorf("get image location: %w", err)
	}
	o.image = image
	return nil
}

// isDfSelected indicates if any Dockerfile is in use.
func (o *initSvcOpts) askDockerfile() (isDfSelected bool, err error) {
	if o.dockerfilePath != "" || o.image != "" {
		return true, nil
	}
	if err = o.dockerEngineValidator.CheckDockerEngineRunning(); err != nil {
		var errDaemon *exec.ErrDockerDaemonNotResponsive
		switch {
		case errors.Is(err, exec.ErrDockerCommandNotFound):
			log.Info("Docker command is not found; Copilot won't build from a Dockerfile.\n")
			return false, nil
		case errors.As(err, &errDaemon):
			log.Info("Docker daemon is not responsive; Copilot won't build from a Dockerfile.\n")
			return false, nil
		default:
			return false, fmt.Errorf("check if docker engine is running: %w", err)
		}
	}
	//os, arch, err := o.dockerEngineValidator.GetPlatform()
	//fmt.Println("os " + os)
	//fmt.Println("arch " + arch)
	//if err != nil {
	//	return false, fmt.Errorf("get OS/Arch from docker: %w", err)
	//}
	//o.platform = fmt.Sprintf("%s/%s", os, arch)
	//fmt.Println(o.platform)
	df, err := o.sel.Dockerfile(
		fmt.Sprintf(fmtWkldInitDockerfilePrompt, color.HighlightUserInput(o.name)),
		fmt.Sprintf(fmtWkldInitDockerfilePathPrompt, color.HighlightUserInput(o.name)),
		wkldInitDockerfileHelpPrompt,
		wkldInitDockerfilePathHelpPrompt,
		func(v interface{}) error {
			return validatePath(afero.NewOsFs(), v)
		},
	)
	if err != nil {
		return false, fmt.Errorf("select Dockerfile: %w", err)
	}
	if df == selector.DockerfilePromptUseImage {
		return false, nil
	}
	o.dockerfilePath = df
	return true, nil
}

func (o *initSvcOpts) askSvcPort() (err error) {
	// See if we can get a healthcheck from the dockerfile.
	o.setupParser(o)

	// If the port flag was set, use that and don't ask.
	if o.port != 0 {
		return nil
	}

	var ports []uint16
	if o.dockerfilePath != "" && o.image == "" {
		// Check for exposed ports.
		ports, err = o.df.GetExposedPorts()
		// Ignore any errors in dockerfile parsing--we'll use the default port instead.
		if err != nil {
			log.Debugln(err.Error())
		}
	}

	defaultPort := defaultSvcPortString
	if o.dockerfilePath != "" {
		switch len(ports) {
		case 0:
			// There were no ports detected, keep the default port prompt.
		case 1:
			o.port = ports[0]
			return nil
		default:
			defaultPort = strconv.Itoa(int(ports[0]))
		}
	}
	// Skip asking if it is a backend service.
	if o.wkldType == manifest.BackendServiceType {
		return nil
	}

	port, err := o.prompt.Get(
		fmt.Sprintf(svcInitSvcPortPrompt, color.Emphasize("port")),
		svcInitSvcPortHelpPrompt,
		validateSvcPort,
		prompt.WithDefaultInput(defaultPort),
		prompt.WithFinalMessage("Port:"),
	)
	if err != nil {
		return fmt.Errorf("get port: %w", err)
	}

	portUint, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return fmt.Errorf("parse port string: %w", err)
	}

	o.port = uint16(portUint)

	return nil
}

func (o *initSvcOpts) parseHealthCheck() (*manifest.ContainerHealthCheck, error) {
	if o.dockerfilePath == "" || o.wkldType != manifest.BackendServiceType {
		return nil, nil
	}
	o.setupParser(o)
	hc, err := o.df.GetHealthCheck()
	if err != nil {
		return nil, fmt.Errorf("get healthcheck from Dockerfile: %s, %w", o.dockerfilePath, err)
	}
	if hc == nil {
		return nil, nil
	}
	return &manifest.ContainerHealthCheck{
		Interval:    &hc.Interval,
		Timeout:     &hc.Timeout,
		StartPeriod: &hc.StartPeriod,
		Retries:     &hc.Retries,
		Command:     hc.Cmd,
	}, nil
}

// RecommendedActions returns follow-up actions the user can take after successfully executing the command.
func (o *initSvcOpts) RecommendedActions() []string {
	return []string{
		fmt.Sprintf("Update your manifest %s to change the defaults.", color.HighlightResource(o.manifestPath)),
		fmt.Sprintf("Run %s to deploy your service to a %s environment.",
			color.HighlightCode(fmt.Sprintf("copilot svc deploy --name %s --env %s", o.name, defaultEnvironmentName)),
			defaultEnvironmentName),
	}
}

// buildSvcInitCmd build the command for creating a new service.
func buildSvcInitCmd() *cobra.Command {
	vars := initSvcVars{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Creates a new service in an application.",
		Long: `Creates a new service in an application.
This command is also run as part of "copilot init".`,
		Example: `
  Create a "frontend" load balanced web service.
  /code $ copilot svc init --name frontend --svc-type "Load Balanced Web Service" --dockerfile ./frontend/Dockerfile

  Create a "subscribers" backend service.
  /code $ copilot svc init --name subscribers --svc-type "Backend Service"`,
		RunE: runCmdE(func(cmd *cobra.Command, args []string) error {
			opts, err := newInitSvcOpts(vars)
			if err != nil {
				return err
			}
			if err := opts.Validate(); err != nil { // validate flags
				return err
			}
			log.Warningln("It's best to run this command in the root of your workspace.")
			if err := opts.Ask(); err != nil {
				return err
			}
			if err := opts.Execute(); err != nil {
				return err
			}
			log.Infoln("Recommended follow-up actions:")
			for _, followup := range opts.RecommendedActions() {
				log.Infof("- %s\n", followup)
			}
			return nil
		}),
	}
	cmd.Flags().StringVarP(&vars.appName, appFlag, appFlagShort, tryReadingAppName(), appFlagDescription)
	cmd.Flags().StringVarP(&vars.name, nameFlag, nameFlagShort, "", svcFlagDescription)
	cmd.Flags().StringVarP(&vars.wkldType, svcTypeFlag, typeFlagShort, "", svcTypeFlagDescription)
	cmd.Flags().StringVarP(&vars.dockerfilePath, dockerFileFlag, dockerFileFlagShort, "", dockerFileFlagDescription)
	cmd.Flags().StringVarP(&vars.image, imageFlag, imageFlagShort, "", imageFlagDescription)

	cmd.Flags().Uint16Var(&vars.port, svcPortFlag, 0, svcPortFlagDescription)

	// Bucket flags by service type.
	requiredFlags := pflag.NewFlagSet("Required Flags", pflag.ContinueOnError)
	requiredFlags.AddFlag(cmd.Flags().Lookup(nameFlag))
	requiredFlags.AddFlag(cmd.Flags().Lookup(svcTypeFlag))
	requiredFlags.AddFlag(cmd.Flags().Lookup(dockerFileFlag))
	requiredFlags.AddFlag(cmd.Flags().Lookup(imageFlag))

	lbWebSvcFlags := pflag.NewFlagSet(manifest.LoadBalancedWebServiceType, pflag.ContinueOnError)
	lbWebSvcFlags.AddFlag(cmd.Flags().Lookup(svcPortFlag))

	backendSvcFlags := pflag.NewFlagSet(manifest.BackendServiceType, pflag.ContinueOnError)
	backendSvcFlags.AddFlag(cmd.Flags().Lookup(svcPortFlag))

	cmd.Annotations = map[string]string{
		// The order of the sections we want to display.
		"sections":                          fmt.Sprintf(`Required,%s`, strings.Join(manifest.ServiceTypes, ",")),
		"Required":                          requiredFlags.FlagUsages(),
		manifest.LoadBalancedWebServiceType: lbWebSvcFlags.FlagUsages(),
		manifest.BackendServiceType:         lbWebSvcFlags.FlagUsages(),
	}
	cmd.SetUsageTemplate(`{{h1 "Usage"}}{{if .Runnable}}
  {{.UseLine}}{{end}}{{$annotations := .Annotations}}{{$sections := split .Annotations.sections ","}}{{if gt (len $sections) 0}}

{{range $i, $sectionName := $sections}}{{h1 (print $sectionName " Flags")}}
{{(index $annotations $sectionName) | trimTrailingWhitespaces}}{{if ne (inc $i) (len $sections)}}

{{end}}{{end}}{{end}}{{if .HasAvailableInheritedFlags}}

{{h1 "Global Flags"}}
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasExample}}

{{h1 "Examples"}}{{code .Example}}{{end}}
`)
	return cmd
}
