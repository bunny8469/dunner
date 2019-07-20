/*
Package docker is the interface of dunner to communicate with the Docker Engine through
methods wrapping over Docker client library.
*/
package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/pkg/term"
	"github.com/leopardslab/dunner/internal/logger"
	"github.com/leopardslab/dunner/internal/util"
	"github.com/spf13/viper"
)

var log = logger.Log

// Step describes the information required to run one task in docker container. It is very similar to the concept
// of docker build of a 'Dockerfile' and then a sequence of commands to be executed in `docker run`.
type Step struct {
	Task      string            // The name of the task that the step corresponds to
	Name      string            // Name given to this step for identification purpose
	Image     string            // Image is the repo name on which Docker containers are built
	Command   []string          // The command which runs on the container and exits
	Commands  [][]string        // The list of commands that are to be run in sequence
	Env       []string          // The list of environment variables to be exported inside the container
	WorkDir   string            // The primary directory on which task is to be run
	Volumes   map[string]string // Volumes that are to be attached to the container
	ExtMounts []mount.Mount     // The directories to be mounted on the container as bind volumes
	Follow    string            // The next task that must be executed if this does go successfully
	Args      []string          // The list of arguments that are to be passed
	User      string            // User that will run the command(s) inside the container, also support user:group
}

// Result stores the output of commands run using `docker exec`
type Result struct {
	Output string
	Error  string
}

// Exec method is used to execute the task described in the corresponding step. It returns an object of the
// struct `Result` with the corresponding output and/or error.
//
// Note: A working internet connection is mandatory for the Docker container to contact Docker Hub to find the image and/or
// corresponding updates.
func (step Step) Exec() error {
	var (
		async   = viper.GetBool("Async")
		dryRun  = viper.GetBool("Dry-run")
		verbose = viper.GetBool("Verbose")
	)

	var (
		hostMountFilepath          = viper.GetString("WorkingDirectory")
		containerDefaultWorkingDir = "/dunner"
		hostMountTarget            = "/dunner"
		defaultCommand             = []string{"tail", "-f", "/dev/null"}
	)

	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatal(err)
	}
	cli.NegotiateAPIVersion(ctx)

	path, err := filepath.Abs(hostMountFilepath)
	if err != nil {
		log.Fatal(err)
	}

	if viper.GetBool("Force-pull") || !checkImageExist(ctx, cli, step.Image) {
		loadingMsg := fmt.Sprintf("Pulling image: '%s'", step.Image)
		var done chan bool
		if !async {
			done = make(chan bool)
			go util.ShowLoadingMessage(
				loadingMsg,
				fmt.Sprintf("Pulled image: '%s'", step.Image),
				&done,
				nil,
			)
		} else {
			log.Info(loadingMsg)
		}

		out, err := cli.ImagePull(ctx, step.Image, types.ImagePullOptions{})
		if err != nil {
			return fmt.Errorf(`docker: failed to pull image %s: %s`, step.Image, err.Error())
		}

		termFd, isTerm := term.GetFdInfo(os.Stdout)
		if verbose {
			if err = jsonmessage.DisplayJSONMessagesStream(out, os.Stdout, termFd, isTerm, nil); err != nil {
				log.Fatal(err)
			}
		} else {
			if err = jsonmessage.DisplayJSONMessagesStream(out, ioutil.Discard, termFd, isTerm, nil); err != nil {
				log.Fatal(err)
			}
		}

		if !async {
			done <- true
		}
		if err = out.Close(); err != nil {
			log.Fatal(err)
		}
	}

	var containerWorkingDir = containerDefaultWorkingDir
	if step.WorkDir != "" {
		if step.WorkDir[0] == '/' {
			containerWorkingDir = step.WorkDir
		} else {
			containerWorkingDir = filepath.Join(hostMountTarget, step.WorkDir)
		}
	}

	resp, err := cli.ContainerCreate(
		ctx,
		&container.Config{
			Image:      step.Image,
			Cmd:        defaultCommand,
			Env:        step.Env,
			WorkingDir: containerWorkingDir,
			User:       step.User,
		},
		&container.HostConfig{
			Mounts: append(step.ExtMounts, mount.Mount{
				Type:   mount.TypeBind,
				Source: path,
				Target: hostMountTarget,
			}),
		},
		nil, "")
	if err != nil {
		log.Fatal(err)
	}

	if len(resp.Warnings) > 0 {
		for warning := range resp.Warnings {
			log.Warn(warning)
		}
	}

	if err = cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		log.Fatal(err)
	}

	defer func() {
		dur, err := time.ParseDuration("-1ns") // Negative duration means no force termination
		if err != nil {
			log.Fatal(err)
		}
		if err = cli.ContainerStop(ctx, resp.ID, &dur); err != nil {
			log.Fatal(err)
		}
	}()

	commands := step.Commands
	if len(commands) == 0 {
		commands = append(commands, step.Command)
	}

	for _, cmd := range commands {
		finishedMsg := fmt.Sprintf(
			"Finished running command '%s' on '%s' docker",
			strings.Join(cmd, " "),
			step.Image,
		)
		var (
			done chan bool
			show chan bool
		)
		if !async {
			done = make(chan bool)
			show = make(chan bool)
			go util.ShowLoadingMessage(
				fmt.Sprintf(
					"Running command '%s' of '%s' task on a container of '%s' image",
					strings.Join(cmd, " "),
					step.Task,
					step.Image,
				),
				finishedMsg,
				&done,
				&show,
			)
		}

		if dryRun {
			continue
		}
		r, err := runCmd(ctx, cli, resp.ID, cmd)
		if !async {
			done <- true
		}
		if async || <-show {
			if async {
				log.Info(finishedMsg)
			}
			if r != nil && r.Output != "" {
				fmt.Printf(`OUT: %s`, r.Output)
			}
			if r != nil && r.Error != "" {
				logger.ErrorOutput(`ERR: %s`, r.Error)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func runCmd(ctx context.Context, cli *client.Client, containerID string, command []string) (*Result, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf(`config: Command cannot be empty`)
	}

	exec, err := cli.ContainerExecCreate(ctx, containerID, types.ExecConfig{
		Cmd:          command,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	resp, err := cli.ContainerExecAttach(ctx, exec.ID, types.ExecStartCheck{})
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Close()

	result := ExtractResult(resp.Reader, command)

	info, err := cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		log.Fatal(err)
	}
	if info.ExitCode != 0 {
		return result, fmt.Errorf("Command execution failed with exit code %d", info.ExitCode)
	}

	return result, nil
}

// ExtractResult can parse output and/or error corresponding to the command passed as an argument,
// from an io.Reader and convert to an object of strings.
func ExtractResult(reader io.Reader, command []string) *Result {
	var out, errOut bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errOut, reader); err != nil {
		log.Fatal(err)
	}

	var result = Result{
		Output: out.String(),
		Error:  errOut.String(),
	}
	return &result
}

func checkImageExist(ctx context.Context, cli *client.Client, image string) bool {
	log.Debugf("docker: checking existence of the image '%s", image)
	var splitImage = strings.Split(image, ":")
	if len(splitImage) == 2 {
		//tag := splitImage[1]
		hostImages, err := cli.ImageList(ctx, types.ImageListOptions{})
		if err != nil {
			log.Error(err)
		}
		for _, imageSummary := range hostImages {
			for _, repoTag := range imageSummary.RepoTags {
				if repoTag == image {
					log.Infof("Image '%s' exists with the host", image)
					return true
				}
			}
		}
	} else if len(splitImage) > 2 {
		log.Errorf(`docker: incorrect format for image name`)
	}
	return false
}
