package cmd

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/briandowns/spinner"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	AppVersion      string
	Environment     string
	PrimaryRegions  []string
	RegionalRegions []string
	DryRun          bool
	SelfDestroy     bool
	Account         string
	LogLevel        string
	Interactive     bool
	Container       string = "docker.io/runiac/deploy:latest-alpine-full"
	Namespace       string
	DeploymentRing  string
	Local           bool
	Runner          string
	PullRequest     string
	StepWhitelist   []string
	Dockerfile      string = ".runiac/Dockerfile"
	ContainerEngine string = "docker"
	Test            bool   = false
)

func init() {
	deployCmd.Flags().StringVarP(&AppVersion, "version", "v", "", "Version of the iac code")
	deployCmd.Flags().StringVarP(&Environment, "environment", "e", "", "Targeted environment")
	deployCmd.Flags().StringVarP(&Account, "account", "a", "", "Targeted Cloud Account (ie. azure subscription, gcp project or aws account)")
	deployCmd.Flags().StringArrayVarP(&PrimaryRegions, "primary-regions", "p", []string{}, "Primary regions")
	deployCmd.Flags().StringArrayVarP(&RegionalRegions, "regional-regions", "r", []string{}, "Runiac will concurrently execute the ./regional directory across these regions setting the runiac_region input variable")
	deployCmd.Flags().BoolVar(&DryRun, "dry-run", false, "Dry Run")
	deployCmd.Flags().BoolVar(&SelfDestroy, "self-destroy", false, "Teardown after running deploy")
	deployCmd.Flags().StringVar(&LogLevel, "log-level", "", "Log level")
	deployCmd.Flags().BoolVar(&Interactive, "interactive", false, "Run Docker container in interactive mode")
	deployCmd.Flags().StringVarP(&Container, "container", "c", Container, "The runiac deploy container to execute in.")
	deployCmd.Flags().StringVarP(&DeploymentRing, "deployment-ring", "d", "", "The deployment ring to configure")
	deployCmd.Flags().BoolVar(&Local, "local", false, "Pre-configure settings to create an isolated configuration specific to the executing machine")
	deployCmd.Flags().StringVarP(&Runner, "runner", "", "terraform", "The deployment tool to use for deploying infrastructure")
	deployCmd.Flags().StringSliceVarP(&StepWhitelist, "steps", "s", []string{}, "Only run the specified steps. To specify steps inside a track: -s {trackName}/{stepName}.  To run multiple steps, separate with a comma.  If empty, it will run all steps. To run no steps, specify a non-existent step.")
	deployCmd.Flags().StringVar(&PullRequest, "pull-request", "", "Pre-configure settings to create an isolated configuration specific to a pull request, provide pull request identifier")
	deployCmd.Flags().StringVarP(&Dockerfile, "dockerfile", "f", Dockerfile, "The dockerfile runiac builds to execute the deploy in, defaults to the autogenerated '%s' and must derive from runiac/deploy:{version}-alpine. Runiac official dockerfiles are here: https://github.com/runiac/docker")
	deployCmd.Flags().StringVar(&ContainerEngine, "container-engine", ContainerEngine, "Container engine (ie. podman or docker)")
	deployCmd.Flags().BoolVar(&Test, "test", Test, "Hidden flag only set during unit testing")
	deployCmd.Flags().MarkHidden("test")

	rootCmd.AddCommand(deployCmd)
}

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy configurations",
	Long:  `This will execute the deploy action for each step.`,
	Run: func(cmd *cobra.Command, args []string) {
		// These options can be set via config file.
		// The command line option, if set, always takes precendence.
		setStringFlag(cmd, &ContainerEngine, "container-engine", "container_engine")
		setStringFlag(cmd, &Container, "container", "container")
		setStringFlag(cmd, &Dockerfile, "dockerfile", "dockerfile")

		// This condition is only met during unit testing.
		// It should come after any setup / option parsing and precendence steps.
		if Test {
			return
		}

		checkDockerExists()

		ok := checkInitialized()
		if !ok {
			fmt.Printf("You need to run 'runiac init' before you can use the CLI in this directory\n")
			return
		}

		buildKit := "DOCKER_BUILDKIT=1"
		containerTag := viper.GetString("project")

		cmdd := exec.Command(ContainerEngine, "build", "-t", containerTag, "-f", Dockerfile)

		cmdd.Args = append(cmdd.Args, getBuildArguments()...)

		logrus.Info(strings.Join(cmdd.Args, " "))

		var stdoutBuf, stderrBuf bytes.Buffer

		cmdd.Env = append(os.Environ(), buildKit)
		s := spinner.New(spinner.CharSets[11], 100*time.Millisecond)
		s.Suffix = " Building project container..."

		if Dockerfile != "" {
			cmdd.Stdout = io.MultiWriter(os.Stdout, &stdoutBuf)
			cmdd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

			err := cmdd.Run()
			if err != nil {
				log.Fatalf("Runiac failed to build %s", Dockerfile)
			}
		} else {
			s.Start()
			b, err := cmdd.CombinedOutput()
			if err != nil {
				s.Stop()
				logrus.Error(string(b))
				logrus.WithError(err).Fatalf("Building project container failed with %s\n", err)
			}

			s.Stop()
		}

		logrus.Info("Completed build, lets run!")

		cmd2 := exec.Command(ContainerEngine, "run", "--rm")

		cmd2.Env = append(os.Environ(), buildKit)

		// pre-configure for local development experience
		if Local {
			namespace, err := getMachineName()

			if err != nil {
				logrus.WithError(err).Fatal(err)
			}

			Namespace = namespace
			DeploymentRing = "local"
		} else if PullRequest != "" {
			Namespace = PullRequest
			DeploymentRing = "pr"
		}

		cmd2.Args = appendEIfSet(cmd2.Args, "DEPLOYMENT_RING", DeploymentRing)
		cmd2.Args = appendEIfSet(cmd2.Args, "RUNNER", Runner)
		cmd2.Args = appendEIfSet(cmd2.Args, "NAMESPACE", Namespace)
		cmd2.Args = appendEIfSet(cmd2.Args, "VERSION", AppVersion)
		cmd2.Args = appendEIfSet(cmd2.Args, "ENVIRONMENT", Environment)
		cmd2.Args = appendEIfSet(cmd2.Args, "DRY_RUN", fmt.Sprintf("%v", DryRun))
		cmd2.Args = appendEIfSet(cmd2.Args, "SELF_DESTROY", fmt.Sprintf("%v", SelfDestroy))
		cmd2.Args = appendEIfSet(cmd2.Args, "STEP_WHITELIST", strings.Join(StepWhitelist, ","))

		if len(PrimaryRegions) > 0 {
			cmd2.Args = appendEIfSet(cmd2.Args, "PRIMARY_REGION", PrimaryRegions[0])
		}

		if len(RegionalRegions) > 0 {
			cmd2.Args = appendEIfSet(cmd2.Args, "REGIONAL_REGIONS", strings.Join(RegionalRegions, ","))
		}
		cmd2.Args = appendEIfSet(cmd2.Args, "ACCOUNT_ID", Account)
		cmd2.Args = appendEIfSet(cmd2.Args, "LOG_LEVEL", LogLevel)

		if Interactive {
			cmd2.Args = append(cmd2.Args, "-it")
		}

		// TODO: how best to allow consumer whitelist environment variables or simply pass all in?
		for _, env := range cmd2.Env {
			if strings.HasPrefix(env, "TF_VAR_") {
				cmd2.Args = append(cmd2.Args, "-e", env)
			}

			if strings.HasPrefix(env, "ARM_") {
				cmd2.Args = append(cmd2.Args, "-e", env)
			}

			if strings.HasPrefix(env, "RUNIAC_") {
				cmd2.Args = append(cmd2.Args, "-e", env)
			}

			if strings.HasPrefix(env, "AWS_") {
				cmd2.Args = append(cmd2.Args, "-e", env)
			}
		}

		// handle local volume maps
		dir, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}

		// persist azure cli between container executions
		cmd2.Args = append(cmd2.Args, "-v", fmt.Sprintf("%s/.runiac/.azure:/root/.azure", dir))

		// persist gcloud cli
		cmd2.Args = append(cmd2.Args, "-v", fmt.Sprintf("%s/.runiac/.config/gcloud:/root/.config/gcloud", dir))

		// persist aws cli
		cmd2.Args = append(cmd2.Args, "-v", fmt.Sprintf("%s/.runiac/.aws:/root/.aws", dir))

		// persist local terraform state between container executions
		cmd2.Args = append(cmd2.Args, "-v", fmt.Sprintf("%s/.runiac/tfstate:/runiac/tfstate", dir))

		cmd2.Args = append(cmd2.Args, containerTag)

		logrus.Info(strings.Join(cmd2.Args, " "))

		cmd2.Stdout = io.MultiWriter(os.Stdout, &stdoutBuf)
		cmd2.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
		cmd2.Stdin = os.Stdin

		err2 := cmd2.Run()
		if err2 != nil {
			log.Fatalf("Running iac failed with %s\n", err2)
		}
	},
}

// setStringFlag - If flag is changed via command line, do nothing, else check config file for value.
func setStringFlag(cmd *cobra.Command, flag *string, cmdLineOption string, configOption string) {
	if cmd.Flags().Changed(cmdLineOption) == false {
		configValue := viper.GetString(configOption)

		if configValue != "" {
			*flag = configValue
		}
	}
}

func appendEIfSet(slice []string, arg string, val string) []string {
	if val != "" {
		return appendE(slice, arg, val)
	} else {
		return slice
	}
}
func appendE(slice []string, arg string, val string) []string {
	return append(slice, "-e", fmt.Sprintf("RUNIAC_%s=%s", arg, val))
}

func checkDockerExists() {
	_, err := exec.LookPath(ContainerEngine)
	if err != nil {
		fmt.Printf("please add '%s' to the path\n", ContainerEngine)
	}
}

func checkInitialized() bool {
	return InitAction()
}

func getBuildArguments() (args []string) {
	// check viper configuration if not set
	if Container == "" && viper.GetString("container") != "" {
		Container = viper.GetString("container")
	}

	if Container != "" {
		args = append(args, "--build-arg", fmt.Sprintf("RUNIAC_CONTAINER=%s", Container))
	}

	// must be last argument added for docker build current directory context
	args = append(args, ".")

	return
}

func getMachineName() (string, error) {
	// This handles most *nix platforms
	username := os.Getenv("USER")
	if username != "" {
		return sanitizeMachineName(username), nil
	}

	// This handles Windows platforms
	username = os.Getenv("USERNAME")
	if username != "" {
		return sanitizeMachineName(username), nil
	}

	// This is for other platforms without ENV vars set above
	cmdd := exec.Command("whoami")

	stdout, err := cmdd.StdoutPipe()
	if err != nil {
		return "", err
	}

	err = cmdd.Start()
	if err != nil {
		return "", err
	}

	out, err := ioutil.ReadAll(stdout)

	if err := cmdd.Wait(); err != nil {
		return "", err
	}

	return sanitizeMachineName(string(out)), err
}

func sanitizeMachineName(s string) string {
	s = strings.TrimSpace(s)
	re := regexp.MustCompile("[^a-zA-Z0-9]")

	return re.ReplaceAllLiteralString(s, "_")
}
