package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/pwaller/barrier"
)

type Container struct {
	Name      string
	Args, Env []string

	client    *docker.Client
	container *docker.Container

	Failed, Superceded, Ready, Closing barrier.Barrier

	wg *sync.WaitGroup

	Errors  <-chan error
	errorsW chan<- error
}

type SourceType int

const (
	// Run build with current directory with context
	BuildCwd            SourceType = iota
	BuildTarballContent            // Build with specified io.Reader as context
	BuildTarballURL                // Build with specified remote URL as context
	DockerPull                     // Run a docker pull to obtain the image
)

type ContainerSource struct {
	Type                SourceType
	buildTarballContent io.Reader
	buildTarballURL     string
	dockerImageName     string
}

// Construct a *Container. When the `wg` WaitGroup is zero, there is nothing
// outstanding (such as firewall rules which need garbage collecting).
func NewContainer(client *docker.Client, name string, wg *sync.WaitGroup) *Container {

	errors := make(chan error)

	c := &Container{
		Name:    name,
		client:  client,
		wg:      wg,
		Errors:  errors,
		errorsW: errors,
	}

	// If the container fails we should assume it should be torn down.
	c.Failed.Forward(&c.Closing)

	return c
}

// Generate a docker image. This can be done through various mechanisms in
// response to an UpdateEvent (see SourceType constant declarations).
func (c *Container) Build(config UpdateEvent) error {
	if config.BuildComplete != nil {
		defer close(config.BuildComplete)
	}

	var err error
	bo := docker.BuildImageOptions{}
	bo.Name = c.Name
	bo.OutputStream = config.OutputStream
	if bo.OutputStream == nil {
		bo.OutputStream = os.Stderr
	}

	switch config.Source.Type {
	case BuildCwd:
		bo.ContextDir, err = os.Getwd()
		if err != nil {
			return err
		}
	case BuildTarballContent:
		bo.InputStream = config.Source.buildTarballContent
	default:
		return fmt.Errorf("Unimplemented ContainerSource: %v", config.Source.Type)
	}

	return c.client.BuildImage(bo)
}

// Pull an image from a docker repository.
func (c *Container) Pull(config UpdateEvent) error {
	if config.BuildComplete != nil {
		defer close(config.BuildComplete)
	}

	pio := docker.PullImageOptions{}
	pio.Repository = config.Source.dockerImageName
	pio.Registry = ""
	pio.Tag = "latest"
	pio.OutputStream = config.OutputStream
	if pio.OutputStream == nil {
		pio.OutputStream = os.Stderr
	}
	pio.RawJSONStream = false

	return c.client.PullImage(pio, docker.AuthConfiguration{})
}

// `docker create` the container.
func (c *Container) Create(source ContainerSource) error {
	opts := docker.CreateContainerOptions{
		Name: c.Name,
		Config: &docker.Config{
			Hostname:     c.Name,
			AttachStdout: true,
			AttachStderr: true,
			Env:          c.Env,
			Cmd:          c.Args,
		},
	}

	switch source.Type {
	case DockerPull:
		opts.Config.Image = source.dockerImageName
	case BuildCwd, BuildTarballContent:
		opts.Config.Image = c.Name
	}

	var err error
	c.container, err = c.client.CreateContainer(opts)

	return err
}

// CopyOutput copies the output of the container to `w` and blocks until
// completion
func (c *Container) CopyOutput() error {

	// TODO(pwaller): at some point move this on to 'c' for configurability?
	w := os.Stderr
	// Blocks until stream closed
	return c.client.AttachToContainer(docker.AttachToContainerOptions{
		Container:    c.container.ID,
		OutputStream: w,
		ErrorStream:  w,
		Logs:         true,
		Stdout:       true,
		Stderr:       true,
		Stream:       true,
	})
}

// :todo(drj): May want to return errors for truly broken containers (timeout).
// Poll for the program inside the container being ready to accept connections
// Returns `true` for success and `false` for failure.
func (c *Container) AwaitListening() bool {

	for _, port := range c.container.NetworkSettings.PortMappingAPI() {
		url := fmt.Sprint("http://", port.IP, ":", port.PublicPort, "/")
		for {
			response, err := http.Get(url)
			if err == nil && response.StatusCode == http.StatusOK {
				return true
			}
			time.Sleep(50 * time.Millisecond)

			select {
			case <-c.Closing.Barrier():
				// If the container has closed, cease waiting
				return false
			default:
			}
		}
	}

	return false
}

// Start the container (and notify it if c.Closing falls)
func (c *Container) Start() error {
	hc := &docker.HostConfig{
		PublishAllPorts: true,
	}
	err := c.client.StartContainer(c.container.ID, hc)
	if err != nil {
		return err
	}

	// Load container.NetworkSettings
	c.container, err = c.client.InspectContainer(c.container.ID)
	if err != nil {
		return err
	}

	// Listen on the Closing barrier and send a kill to the container if it
	// falls.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		<-c.Closing.Barrier()
		// If the container is signaled to close, send a kill signal
		err := c.client.KillContainer(docker.KillContainerOptions{
			ID: c.container.ID,
		})
		if err == nil {
			return
		}
		switch err := err.(type) {
		case *docker.NoSuchContainer:
			// The container already went away, who cares.
			return
		default:
			log.Println("Killing container failed:", c.container.ID, err)
		}
	}()
	return nil
}

// Wait until container exits
func (c *Container) Wait() (int, error) {
	return c.client.WaitContainer(c.container.ID)
}

// Internal function for raising an error.
func (c *Container) err(err error) {
	c.errorsW <- err
	c.Closing.Fall()
}

// Manage the whole lifecycle of the container in response to a request to
// start it.
func (c *Container) Run(event UpdateEvent) (int, error) {

	defer c.Closing.Fall()
	defer close(c.errorsW)

	switch event.Source.Type {
	case DockerPull:
		err := c.Pull(event)
		if err != nil {
			return -2, err
		}
	case BuildTarballContent, BuildCwd:
		err := c.Build(event)
		if err != nil {
			return -2, err
		}
	}

	err := c.Create(event.Source)
	if err != nil {
		return -1, err
	}
	defer c.Delete()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		err := c.CopyOutput()
		if err != nil {
			c.err(err)
		}
	}()

	err = c.Start()
	if err != nil {
		return -1, err
	}

	go func() {
		if !c.AwaitListening() {
			c.Failed.Fall()
			return
		}
		c.Ready.Fall()
	}()

	return c.Wait()
}

func (c *Container) Delete() {
	err := c.client.RemoveContainer(docker.RemoveContainerOptions{
		ID:            c.container.ID,
		RemoveVolumes: true,
		Force:         true,
	})
	if err != nil {
		log.Println("Warn: failed to delete container:", err)
	}
}
