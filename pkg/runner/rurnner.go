/*
copyright 2020 the Goployer authors

licensed under the apache license, version 2.0 (the "license");
you may not use this file except in compliance with the license.
you may obtain a copy of the license at

    http://www.apache.org/licenses/license-2.0

unless required by applicable law or agreed to in writing, software
distributed under the license is distributed on an "as is" basis,
without warranties or conditions of any kind, either express or implied.
see the license for the specific language governing permissions and
limitations under the license.
*/

package runner

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/GwonsooLee/kubenx/pkg/color"
	Logger "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/DevopsArtFactory/goployer/pkg/aws"
	"github.com/DevopsArtFactory/goployer/pkg/builder"
	"github.com/DevopsArtFactory/goployer/pkg/collector"
	"github.com/DevopsArtFactory/goployer/pkg/constants"
	"github.com/DevopsArtFactory/goployer/pkg/deployer"
	"github.com/DevopsArtFactory/goployer/pkg/initializer"
	"github.com/DevopsArtFactory/goployer/pkg/inspector"
	"github.com/DevopsArtFactory/goployer/pkg/schemas"
	"github.com/DevopsArtFactory/goployer/pkg/slack"
	"github.com/DevopsArtFactory/goployer/pkg/tool"
)

type Runner struct {
	Logger     *Logger.Logger
	Builder    builder.Builder
	Collector  collector.Collector
	Slacker    slack.Slack
	FuncMapper map[string]func() error
}

// SetupBuilder setup builder struct for configuration
func SetupBuilder(mode string) (builder.Builder, error) {
	// Create new builder
	builderSt, err := builder.NewBuilder(nil)
	if err != nil {
		return builder.Builder{}, err
	}

	if !checkManifestCommands(mode) {
		return builderSt, nil
	}

	if err := builderSt.PreConfigValidation(); err != nil {
		return builderSt, err
	}

	builderSt, err = setManifestToBuilder(builderSt)
	if err != nil {
		return builder.Builder{}, err
	}

	m, err := builder.ParseMetricConfig(builderSt.Config.DisableMetrics, constants.MetricYamlPath)
	if err != nil {
		return builder.Builder{}, err
	}

	builderSt.MetricConfig = m

	return builderSt, nil
}

// ServerSetup setup a goployer server
func ServerSetup(config schemas.Config) (builder.Builder, error) {
	// Create new builder
	builderSt, err := builder.NewBuilder(&config)
	if err != nil {
		return builder.Builder{}, err
	}

	builderSt, err = setManifestToBuilder(builderSt)
	if err != nil {
		return builder.Builder{}, err
	}

	m, err := builder.ParseMetricConfig(builderSt.Config.DisableMetrics, constants.MetricYamlPath)
	if err != nil {
		return builder.Builder{}, err
	}

	builderSt.MetricConfig = m

	return builderSt, nil
}

// setManifestToBuilder creates builderSt with manifest configurations
func setManifestToBuilder(builderSt builder.Builder) (builder.Builder, error) {
	if !strings.HasPrefix(builderSt.Config.Manifest, constants.S3Prefix) {
		builderSt = builderSt.SetManifestConfig()
	} else {
		s := aws.BootstrapManifestService(builderSt.Config.ManifestS3Region, "")
		fileBytes, err := s.S3Service.GetManifest(FilterS3Path(builderSt.Config.Manifest))
		if err != nil {
			return builder.Builder{}, err
		}
		builderSt = builderSt.SetManifestConfigWithS3(fileBytes)
	}

	return builderSt, nil
}

// Initialize creates necessary files for goployer
func Initialize(args []string) error {
	var appName string
	var err error

	// validation
	if len(args) > 1 {
		return errors.New("usage: goployer init <application name>")
	}

	if len(args) == 0 {
		appName, err = askApplicationName()
		if err != nil {
			return err
		}
	} else {
		appName = args[0]
	}

	i := initializer.NewInitializer(appName)
	i.Logger.SetLevel(constants.LogLevelMapper[viper.GetString("log-level")])

	if err := i.RunInit(); err != nil {
		return err
	}

	return nil
}

// AddManifest creates single manifest file
func AddManifest(args []string) error {
	var appName string
	var err error

	// validation
	if len(args) > 1 {
		return errors.New("usage: goployer add <application name>")
	}

	if len(args) == 0 {
		appName, err = askApplicationName()
		if err != nil {
			return err
		}
	} else {
		appName = args[0]
	}

	i := initializer.NewInitializer(appName)
	i.Logger.SetLevel(constants.LogLevelMapper[viper.GetString("log-level")])

	if err := i.RunAdd(); err != nil {
		return err
	}

	return nil
}

// Start function is the starting point of all processes.
func Start(builderSt builder.Builder, mode string) error {
	if checkManifestCommands(mode) {
		// Check validation of configurations
		if err := builderSt.CheckValidation(); err != nil {
			return err
		}
	}

	// run with runner
	return withRunner(builderSt, mode, func(slacker slack.Slack) error {
		// These are post actions after deployment
		if !builderSt.Config.SlackOff {
			if mode == "deploy" {
				slacker.SendSimpleMessage(fmt.Sprintf(":100: Deployment is done: %s", builderSt.AwsConfig.Name))
			}

			if mode == "delete" {
				slacker.SendSimpleMessage(fmt.Sprintf(":100: Delete process is done: %s", builderSt.AwsConfig.Name))
			}
		}

		return nil
	})
}

// withRunner creates runner and runs the deployment process
func withRunner(builderSt builder.Builder, mode string, postAction func(slacker slack.Slack) error) error {
	runner, err := NewRunner(builderSt, mode)
	if err != nil {
		return err
	}
	runner.LogFormatting(builderSt.Config.LogLevel)

	if err := runner.Run(mode); err != nil {
		return err
	}

	return postAction(runner.Slacker)
}

// NewRunner creates a new runner
func NewRunner(newBuilder builder.Builder, mode string) (Runner, error) {
	newRunner := Runner{
		Logger:  Logger.New(),
		Builder: newBuilder,
		Slacker: slack.NewSlackClient(newBuilder.Config.SlackOff),
	}

	if checkManifestCommands(mode) {
		newRunner.Collector = collector.NewCollector(newBuilder.MetricConfig, newBuilder.Config.AssumeRole)
	}

	newRunner.FuncMapper = map[string]func() error{
		"deploy": newRunner.Deploy,
		"delete": newRunner.Delete,
		"status": newRunner.Status,
		"update": newRunner.Update,
	}

	return newRunner, nil
}

// LogFormatting sets log format
func (r Runner) LogFormatting(logLevel string) {
	r.Logger.SetOutput(os.Stdout)
	r.Logger.SetLevel(constants.LogLevelMapper[logLevel])
}

// Run executes all required steps for deployments
func (r Runner) Run(mode string) error {
	f, ok := r.FuncMapper[mode]
	if !ok {
		return fmt.Errorf("no function exists to run for %s", mode)
	}
	return f()
}

// Deploy is the main function of `goployer deploy`
func (r Runner) Deploy() error {
	out := os.Stdout
	defer func() {
		if err := recover(); err != nil {
			Logger.Error(err)
			os.Exit(1)
		}
	}()

	if err := r.LocalCheck("Do you really want to deploy this application? "); err != nil {
		return err
	}

	//Send Beginning Message
	r.Logger.Info("Beginning deployment: ", r.Builder.AwsConfig.Name)

	if err := r.Builder.PrintSummary(out, r.Builder.Config.Stack, r.Builder.Config.Region); err != nil {
		return err
	}

	if r.Slacker.ValidClient() {
		r.Logger.Debug("slack configuration is valid")
		var stacks []schemas.Stack
		for _, s := range r.Builder.Stacks {
			if len(r.Builder.Config.Stack) == 0 || r.Builder.Config.Stack == s.Stack {
				stacks = append(stacks, s)
			}
		}
		if err := r.Slacker.SendSummaryMessage(r.Builder.Config, stacks, r.Builder.AwsConfig.Name); err != nil {
			r.Logger.Warn(err.Error())
			r.Slacker.SlackOff = true
		}
	} else if !r.Builder.Config.SlackOff {
		// Slack variables are not set
		r.Logger.Warnln("no slack variables exists. [ SLACK_TOKEN, SLACK_CHANNEL or SLACK_WEBHOOK_URL ]")
	}

	if r.Builder.MetricConfig.Enabled {
		if err := r.CheckEnabledMetrics(); err != nil {
			return err
		}
	}

	r.Logger.Debugf("create wait group for deployer setup")
	wg := sync.WaitGroup{}

	//Prepare deployers
	r.Logger.Debug("create deployers for stacks")
	deployers := []deployer.DeployManager{}
	for _, stack := range r.Builder.Stacks {
		if r.Builder.Config.Stack != "" && stack.Stack != r.Builder.Config.Stack {
			r.Logger.Debugf("Skipping this stack, stack=%s", stack.Stack)
			continue
		}

		r.Logger.Debugf("add deployer setup function : %s", stack.Stack)
		deployers = append(deployers, getDeployer(r.Logger, stack, r.Builder.AwsConfig, r.Builder.APITestTemplates, r.Builder.Config.Region, r.Slacker, r.Collector))
	}

	r.Logger.Debugf("successfully assign deployer to stacks")

	// Check Previous Version
	for _, d := range deployers {
		wg.Add(1)
		go func(deployer deployer.DeployManager) {
			defer wg.Done()
			if err := deployer.CheckPrevious(r.Builder.Config); err != nil {
				r.Logger.Errorf("[StepCheckPrevious] check previous deployer error occurred: %s", err.Error())
			}

			if err := deployer.Deploy(r.Builder.Config); err != nil {
				r.Logger.Errorf("[StepDeploy] deploy step error occurred: %s", err.Error())
			}
		}(d)
	}

	wg.Wait()

	// healthcheck
	if err := doHealthchecking(deployers, r.Builder.Config, r.Logger); err != nil {
		return err
	}

	// Attach scaling policy
	for _, d := range deployers {
		wg.Add(1)
		go func(deployer deployer.DeployManager) {
			defer wg.Done()
			if err := deployer.FinishAdditionalWork(r.Builder.Config); err != nil {
				r.Logger.Errorf(err.Error())
			}

			if err := deployer.TriggerLifecycleCallbacks(r.Builder.Config); err != nil {
				r.Logger.Errorf(err.Error())
			}

			if err := deployer.CleanPreviousVersion(r.Builder.Config); err != nil {
				r.Logger.Errorf(err.Error())
			}
		}(d)
	}

	wg.Wait()

	// Checking all previous version before delete asg
	if err := cleanChecking(deployers, r.Builder.Config, r.Logger); err != nil {
		return err
	}

	// gather metrics of previous version
	for _, d := range deployers {
		wg.Add(1)
		go func(deployer deployer.DeployManager) {
			defer wg.Done()
			if err := deployer.GatherMetrics(r.Builder.Config); err != nil {
				r.Logger.Errorf(err.Error())
			}
		}(d)
	}
	wg.Wait()

	// API Test
	for _, d := range deployers {
		wg.Add(1)
		go func(deployer deployer.DeployManager) {
			defer wg.Done()
			if err := deployer.RunAPITest(r.Builder.Config); err != nil {
				r.Logger.Errorf("API test error occurred: %s", err.Error())
			}
		}(d)
	}
	wg.Wait()

	return nil
}

// Delete is the main function for `goployer delete`
func (r Runner) Delete() error {
	defer func() {
		if err := recover(); err != nil {
			Logger.Error(err)
			os.Exit(1)
		}
	}()

	if err := r.LocalCheck("Do you really want to delete applications? "); err != nil {
		return err
	}

	//Send Beginning Message
	r.Logger.Info("Beginning delete process: ", r.Builder.AwsConfig.Name)
	r.Builder.Config.SlackOff = true

	if r.Builder.MetricConfig.Enabled {
		if err := r.CheckEnabledMetrics(); err != nil {
			return err
		}
	}

	wg := sync.WaitGroup{}

	//Prepare deployers
	r.Logger.Debug("create deployers for stacks to delete")
	deployers := []deployer.DeployManager{}
	for _, stack := range r.Builder.Stacks {
		// If target stack is passed from command, then
		// Skip other stacks
		if r.Builder.Config.Stack != "" && stack.Stack != r.Builder.Config.Stack {
			r.Logger.Debugf("Skipping this stack, stack=%s", stack.Stack)
			continue
		}

		r.Logger.Debugf("add deployer setup function : %s", stack.Stack)
		d := getDeployer(r.Logger, stack, r.Builder.AwsConfig, r.Builder.APITestTemplates, r.Builder.Config.Region, r.Slacker, r.Collector)
		deployers = append(deployers, d)
	}

	r.Logger.Debugf("successfully assign deployer to stacks")

	// Check Previous Version
	for _, d := range deployers {
		wg.Add(1)
		go func(deployer deployer.DeployManager) {
			defer wg.Done()
			if err := deployer.CheckPrevious(r.Builder.Config); err != nil {
				r.Logger.Errorf("[StepCheckPrevious] check previous deployer error occurred: %s", err.Error())
			}

			deployer.SkipDeployStep()

			// Trigger Lifecycle Callbacks
			if err := deployer.TriggerLifecycleCallbacks(r.Builder.Config); err != nil {
				r.Logger.Errorf(err.Error())
			}

			// Clear previous Version
			if err := deployer.CleanPreviousVersion(r.Builder.Config); err != nil {
				r.Logger.Errorf(err.Error())
			}
		}(d)
	}
	wg.Wait()

	// Checking all previous version before delete asg
	if err := cleanChecking(deployers, r.Builder.Config, r.Logger); err != nil {
		return err
	}

	// gather metrics of previous version
	for _, d := range deployers {
		wg.Add(1)
		go func(deployer deployer.DeployManager) {
			defer wg.Done()
			if err := deployer.GatherMetrics(r.Builder.Config); err != nil {
				r.Logger.Errorf(err.Error())
			}
		}(d)
	}
	wg.Wait()

	return nil
}

// Status shows the detailed information about autoscaling deployment
func (r Runner) Status() error {
	inspector := inspector.New(r.Builder.Config.Region)

	asg, err := inspector.SelectStack(r.Builder.Config.Application)
	if err != nil {
		return err
	}

	group, err := inspector.GetStackInformation(asg)
	if err != nil {
		return err
	}

	launchTemplateInfo, err := inspector.GetLaunchTemplateInformation(*group.LaunchTemplate.LaunchTemplateId)
	if err != nil {
		return err
	}

	securityGroups, err := inspector.GetSecurityGroupsInformation(launchTemplateInfo.LaunchTemplateData.SecurityGroupIds)
	if err != nil {
		return err
	}

	inspector.StatusSummary = inspector.SetStatusSummary(group, securityGroups)

	if err := inspector.Print(); err != nil {
		return err
	}

	return nil
}

// Update will changes configuration of current deployment on live
func (r Runner) Update() error {
	i := inspector.New(r.Builder.Config.Region)

	asg, err := i.SelectStack(r.Builder.Config.Application)
	if err != nil {
		return err
	}

	group, err := i.GetStackInformation(asg)
	if err != nil {
		return err
	}

	oldCapacity := makeCapacityStruct(*group.MinSize, *group.MaxSize, *group.DesiredCapacity)
	newCapacity := makeCapacityStruct(nullCheck(r.Builder.Config.Min, oldCapacity.Min), nullCheck(r.Builder.Config.Max, oldCapacity.Max), nullCheck(r.Builder.Config.Desired, oldCapacity.Desired))
	if err := CheckUpdateInformation(oldCapacity, newCapacity); err != nil {
		return err
	}
	color.Cyan.Fprintln(os.Stdout, "[ AS IS ]")
	color.Cyan.Fprintf(os.Stdout, "Min: %d, Desired: %d, Max: %d", oldCapacity.Min, oldCapacity.Desired, oldCapacity.Max)
	color.Green.Fprintln(os.Stdout, "[ TO BE ]")
	color.Green.Fprintf(os.Stdout, "Min: %d, Desired: %d, Max: %d", newCapacity.Min, newCapacity.Desired, newCapacity.Max)

	if err := r.LocalCheck("Do you really want to update? "); err != nil {
		return err
	}

	if oldCapacity.Desired > newCapacity.Desired {
		r.Logger.Debugf("downsizing operation is triggered")
	}

	i.UpdateFields = inspector.UpdateFields{
		AutoscalingName: *group.AutoScalingGroupName,
		Capacity:        newCapacity,
	}

	r.Logger.Debugf("start updating configuration")
	if err := i.Update(); err != nil {
		return err
	}
	r.Logger.Debugf("update configuration is triggered")

	stack := i.GenerateStack(r.Builder.Config.Region, group)
	r.Builder.Config.DownSizingUpdate = oldCapacity.Desired > newCapacity.Desired
	r.Builder.Config.TargetAutoscalingGroup = i.UpdateFields.AutoscalingName
	r.Builder.Config.ForceManifestCapacity = false

	r.Logger.Debugf("create deployer for update")
	deployers := []deployer.DeployManager{
		getDeployer(r.Logger, stack, r.Builder.AwsConfig, r.Builder.APITestTemplates, r.Builder.Config.Region, r.Slacker, r.Collector),
	}

	// healthcheck
	r.Logger.Debugf("start healthchecking")
	if err := doHealthchecking(deployers, r.Builder.Config, r.Logger); err != nil {
		return err
	}
	r.Logger.Debugf("healthcheck process is done")

	r.Logger.Infof("update operation is finished")

	return nil
}

//Generate new deployer
func getDeployer(logger *Logger.Logger, stack schemas.Stack, awsConfig schemas.AWSConfig, apiTestTemplates []*schemas.APITestTemplate, region string, slack slack.Slack, c collector.Collector) deployer.DeployManager {
	var att *schemas.APITestTemplate
	if stack.APITestEnabled {
		for _, at := range apiTestTemplates {
			if at.Name == stack.APITestTemplate {
				att = at
				break
			}
		}
	}

	deployer := deployer.NewBlueGrean(
		stack.ReplacementType,
		logger,
		awsConfig,
		att,
		stack,
		region,
	)

	deployer.Slack = slack
	deployer.Collector = c

	return deployer
}

// doHealthchecking checks if newly deployed autoscaling group is healthy
func doHealthchecking(deployers []deployer.DeployManager, config schemas.Config, logger *Logger.Logger) error {
	healthyStackList := []string{}
	healthy := false

	ch := make(chan map[string]bool)

	for !healthy {
		count := 0

		logger.Debugf("Start Timestamp: %d, timeout: %s", config.StartTimestamp, config.Timeout)
		isTimeout, _ := tool.CheckTimeout(config.StartTimestamp, config.Timeout)
		if isTimeout {
			return fmt.Errorf("timeout has been exceeded : %.0f minutes", config.Timeout.Minutes())
		}

		for _, d := range deployers {
			if tool.IsStringInArray(d.GetStackName(), healthyStackList) {
				continue
			}

			count++

			//Start healthcheck thread
			go func(deployer deployer.DeployManager) {
				ch <- deployer.HealthChecking(config)
			}(d)
		}

		for count > 0 {
			ret := <-ch
			if ret["error"] {
				return errors.New("error happened while healthchecking")
			}
			for key, val := range ret {
				if key == "error" {
					continue
				}
				if val {
					healthyStackList = append(healthyStackList, key)
				}
			}
			count--
		}

		if len(healthyStackList) == len(deployers) {
			logger.Info("All stacks are healthy")
			healthy = true
		} else {
			logger.Info("All stacks are not healthy... Please waiting to be deployed...")
			time.Sleep(config.PollingInterval)
		}
	}

	return nil
}

// cleanChecking cleans old autoscaling groups
func cleanChecking(deployers []deployer.DeployManager, config schemas.Config, logger *Logger.Logger) error {
	doneStackList := []string{}
	done := false

	ch := make(chan map[string]bool)

	for !done {
		count := 0
		for _, d := range deployers {
			if tool.IsStringInArray(d.GetStackName(), doneStackList) {
				continue
			}

			count++

			//Start terminateChecking thread
			go func(deployer deployer.DeployManager) {
				ch <- deployer.TerminateChecking(config)
			}(d)
		}

		for count > 0 {
			ret := <-ch
			for stack, fin := range ret {
				if fin {
					logger.Debug("Finished stack : ", stack)
					doneStackList = append(doneStackList, stack)
				}
			}
			count--
		}

		if len(doneStackList) == len(deployers) {
			logger.Info("All stacks are terminated!!")
			done = true
		} else {
			logger.Info("All stacks are not ready to be terminated... Please waiting...")
			time.Sleep(config.PollingInterval)
		}
	}

	return nil
}

// CheckEnabledMetrics checks if metrics configuration is enabled or not
func (r Runner) CheckEnabledMetrics() error {
	r.Logger.Infof("Metric Measurement is enabled")

	r.Logger.Debugf("check if storage exists or not")
	if err := r.Collector.CheckStorage(r.Logger); err != nil {
		return err
	}

	return nil
}

// FilterS3Path detects s3 path
func FilterS3Path(path string) (string, string) {
	path = strings.ReplaceAll(path, constants.S3Prefix, "")
	split := strings.Split(path, "/")

	return split[0], strings.Join(split[1:], "/")
}

// askApplicationName gets application name from interactive terminal
func askApplicationName() (string, error) {
	var answer string
	prompt := &survey.Input{
		Message: "What is application name? ",
	}
	survey.AskOne(prompt, &answer)
	if answer == constants.EmptyString {
		return constants.EmptyString, errors.New("canceled")
	}

	return answer, nil
}

// checkManifestCommands checks if mode is needed to run manifest validation
func checkManifestCommands(mode string) bool {
	return tool.IsStringInArray(mode, []string{"deploy", "delete"})
}

func (r Runner) LocalCheck(message string) error {
	// From local os, you need to ensure that this command is intended
	if runtime.GOOS == "darwin" && !r.Builder.Config.AutoApply {
		if !tool.AskContinue(message) {
			return errors.New("you declined to run command")
		}
	}
	return nil
}

// CheckUpdateInformation checks if updated information is valid or not
func CheckUpdateInformation(old, new schemas.Capacity) error {
	if new.Min > new.Max {
		return errors.New("minimum value cannot be larger than maximum value")
	}

	if new.Min > new.Desired {
		return errors.New("desired value cannot be smaller than maximum value")
	}

	if new.Desired > new.Max {
		return errors.New("desired value cannot be larger than max value")
	}

	if old == new {
		return errors.New("nothing is updated")
	}
	return nil
}

// makeCapacityStruct creates schemas.Capacity with values
func makeCapacityStruct(min, max, desired int64) schemas.Capacity {
	return schemas.Capacity{
		Min:     min,
		Max:     max,
		Desired: desired,
	}
}

// nullCheck will return original value if no input exists
func nullCheck(input, origin int64) int64 {
	if input < 0 {
		return origin
	}

	return input
}
