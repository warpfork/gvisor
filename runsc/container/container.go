// Copyright 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package container creates and manipulates containers.
package container

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.googlesource.com/gvisor/pkg/log"
	"gvisor.googlesource.com/gvisor/pkg/sentry/control"
	"gvisor.googlesource.com/gvisor/runsc/boot"
	"gvisor.googlesource.com/gvisor/runsc/sandbox"
	"gvisor.googlesource.com/gvisor/runsc/specutils"
)

// metadataFilename is the name of the metadata file relative to the container
// root directory that holds sandbox metadata.
const metadataFilename = "meta.json"

// validateID validates the container id.
func validateID(id string) error {
	// See libcontainer/factory_linux.go.
	idRegex := regexp.MustCompile(`^[\w+-\.]+$`)
	if !idRegex.MatchString(id) {
		return fmt.Errorf("invalid container id: %v", id)
	}
	return nil
}

// Container represents a containerized application. When running, the
// container is associated with a single Sandbox.
//
// Container metadata can be saved and loaded to disk. Within a root directory,
// we maintain subdirectories for each container named with the container id.
// The container metadata is is stored as json within the container directory
// in a file named "meta.json". This metadata format is defined by us, and is
// not part of the OCI spec.
//
// Containers must write their metadata file after any change to their internal
// state. The entire container directory is deleted when the container is
// destroyed.
type Container struct {
	// ID is the container ID.
	ID string `json:"id"`

	// Spec is the OCI runtime spec that configures this container.
	Spec *specs.Spec `json:"spec"`

	// BundleDir is the directory containing the container bundle.
	BundleDir string `json:"bundleDir"`

	// Root is the directory containing the container metadata file.
	Root string `json:"root"`

	// CreatedAt is the time the container was created.
	CreatedAt time.Time `json:"createdAt"`

	// Owner is the container owner.
	Owner string `json:"owner"`

	// ConsoleSocket is the path to a unix domain socket that will receive
	// the console FD. It is only used during create, so we don't need to
	// store it in the metadata.
	ConsoleSocket string `json:"-"`

	// Status is the current container Status.
	Status Status `json:"status"`

	// Sandbox is the sandbox this container is running in. It will be nil
	// if the container is not in state Running or Created.
	Sandbox *sandbox.Sandbox `json:"sandbox"`
}

// Load loads a container with the given id from a metadata file.
func Load(rootDir, id string) (*Container, error) {
	log.Debugf("Load container %q %q", rootDir, id)
	if err := validateID(id); err != nil {
		return nil, err
	}
	cRoot := filepath.Join(rootDir, id)
	if !exists(cRoot) {
		return nil, fmt.Errorf("container with id %q does not exist", id)
	}
	metaFile := filepath.Join(cRoot, metadataFilename)
	if !exists(metaFile) {
		return nil, fmt.Errorf("container with id %q does not have metadata file %q", id, metaFile)
	}
	metaBytes, err := ioutil.ReadFile(metaFile)
	if err != nil {
		return nil, fmt.Errorf("error reading container metadata file %q: %v", metaFile, err)
	}
	var c Container
	if err := json.Unmarshal(metaBytes, &c); err != nil {
		return nil, fmt.Errorf("error unmarshaling container metadata from %q: %v", metaFile, err)
	}

	// If the status is "Running" or "Created", check that the sandbox
	// process still exists, and set it to Stopped if it does not.
	//
	// This is inherently racey.
	if c.Status == Running || c.Status == Created {
		// Check if the sandbox process is still running.
		if c.Sandbox.IsRunning() {
			// TODO: Send a message into the sandbox to
			// see if this particular container is still running.
		} else {
			// Sandbox no longer exists, so this container
			// definitly does not exist.
			c.Status = Stopped
			c.Sandbox = nil
		}
	}

	return &c, nil
}

// List returns all container ids in the given root directory.
func List(rootDir string) ([]string, error) {
	log.Debugf("List containers %q", rootDir)
	fs, err := ioutil.ReadDir(rootDir)
	if err != nil {
		return nil, fmt.Errorf("ReadDir(%s) failed: %v", rootDir, err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.Name())
	}
	return out, nil
}

// Create creates the container in a new Sandbox process, unless the metadata
// indicates that an existing Sandbox should be used.
func Create(id string, spec *specs.Spec, conf *boot.Config, bundleDir, consoleSocket, pidFile string) (*Container, error) {
	log.Debugf("Create container %q in root dir: %s", id, conf.RootDir)
	if err := validateID(id); err != nil {
		return nil, err
	}
	if err := specutils.ValidateSpec(spec); err != nil {
		return nil, err
	}

	containerRoot := filepath.Join(conf.RootDir, id)
	if exists(containerRoot) {
		return nil, fmt.Errorf("container with id %q already exists: %q ", id, containerRoot)
	}

	c := &Container{
		ID:            id,
		Spec:          spec,
		ConsoleSocket: consoleSocket,
		BundleDir:     bundleDir,
		Root:          containerRoot,
		Status:        Creating,
		Owner:         os.Getenv("USER"),
	}

	// TODO: If the metadata annotations indicates that this
	// container should be started in another sandbox, we must do so. The
	// metadata will indicate the ID of the sandbox, which is the same as
	// the ID of the init container in the sandbox. We can look up that
	// init container by ID to get the sandbox, then we need to expose a
	// way to run a new container in the sandbox.

	// Start a new sandbox for this container. Any errors after this point
	// must destroy the container.
	s, err := sandbox.Create(id, spec, conf, bundleDir, consoleSocket)
	if err != nil {
		c.Destroy()
		return nil, err
	}

	c.Sandbox = s
	c.Status = Created

	// Save the metadata file.
	if err := c.save(); err != nil {
		c.Destroy()
		return nil, err
	}

	// Write the pid file. Containerd considers the create complete after
	// this file is created, so it must be the last thing we do.
	if pidFile != "" {
		if err := ioutil.WriteFile(pidFile, []byte(strconv.Itoa(c.Pid())), 0644); err != nil {
			s.Destroy()
			return nil, fmt.Errorf("error writing pid file: %v", err)
		}
	}

	return c, nil
}

// Start starts running the containerized process inside the sandbox.
func (c *Container) Start(conf *boot.Config) error {
	log.Debugf("Start container %q", c.ID)
	if c.Status != Created {
		return fmt.Errorf("cannot start container in state %s", c.Status)
	}

	// "If any prestart hook fails, the runtime MUST generate an error,
	// stop and destroy the container".
	if c.Spec.Hooks != nil {
		if err := executeHooks(c.Spec.Hooks.Prestart, c.State()); err != nil {
			c.Destroy()
			return err
		}
	}

	if err := c.Sandbox.Start(c.ID, c.Spec, conf); err != nil {
		c.Destroy()
		return err
	}

	// "If any poststart hook fails, the runtime MUST log a warning, but
	// the remaining hooks and lifecycle continue as if the hook had
	// succeeded".
	if c.Spec.Hooks != nil {
		executeHooksBestEffort(c.Spec.Hooks.Poststart, c.State())
	}

	c.Status = Running
	return c.save()
}

// Run is a helper that calls Create + Start + Wait.
func Run(id string, spec *specs.Spec, conf *boot.Config, bundleDir, consoleSocket, pidFile string) (syscall.WaitStatus, error) {
	log.Debugf("Run container %q in root dir: %s", id, conf.RootDir)
	c, err := Create(id, spec, conf, bundleDir, consoleSocket, pidFile)
	if err != nil {
		return 0, fmt.Errorf("error creating container: %v", err)
	}
	if err := c.Start(conf); err != nil {
		return 0, fmt.Errorf("error starting container: %v", err)
	}
	return c.Wait()
}

// Execute runs the specified command in the container.
func (c *Container) Execute(e *control.ExecArgs) (syscall.WaitStatus, error) {
	log.Debugf("Execute in container %q, args: %+v", c.ID, e)
	if c.Status != Created && c.Status != Running {
		return 0, fmt.Errorf("cannot exec in container in state %s", c.Status)
	}
	return c.Sandbox.Execute(c.ID, e)
}

// Event returns events for the container.
func (c *Container) Event() (*boot.Event, error) {
	log.Debugf("Getting events for container %q", c.ID)
	if c.Status != Running && c.Status != Created {
		return nil, fmt.Errorf("cannot get events for container in state: %s", c.Status)
	}
	return c.Sandbox.Event(c.ID)
}

// Pid returns the Pid of the sandbox the container is running in, or -1 if the
// container is not running.
func (c *Container) Pid() int {
	if c.Status != Running && c.Status != Created {
		return -1
	}
	return c.Sandbox.Pid
}

// Wait waits for the container to exit, and returns its WaitStatus.
func (c *Container) Wait() (syscall.WaitStatus, error) {
	log.Debugf("Wait on container %q", c.ID)
	return c.Sandbox.Wait(c.ID)
}

// Signal sends the signal to the container.
func (c *Container) Signal(sig syscall.Signal) error {
	log.Debugf("Signal container %q", c.ID)
	if c.Status == Stopped {
		log.Warningf("container %q not running, not sending signal %v", c.ID, sig)
		return nil
	}
	return c.Sandbox.Signal(c.ID, sig)
}

// State returns the metadata of the container.
func (c *Container) State() specs.State {
	return specs.State{
		Version: specs.Version,
		ID:      c.ID,
		Status:  c.Status.String(),
		Pid:     c.Pid(),
		Bundle:  c.BundleDir,
	}
}

// Processes retrieves the list of processes and associated metadata inside a
// container.
func (c *Container) Processes() ([]*control.Process, error) {
	if c.Status != Running {
		return nil, fmt.Errorf("cannot get processes of container %q because it isn't running. It is in state %v", c.ID, c.Status)
	}
	return c.Sandbox.Processes(c.ID)
}

// Destroy frees all resources associated with the container.
func (c *Container) Destroy() error {
	log.Debugf("Destroy container %q", c.ID)

	// First stop the container.
	if err := c.Sandbox.Stop(c.ID); err != nil {
		return err
	}

	// Then destroy all the metadata.
	if err := os.RemoveAll(c.Root); err != nil {
		log.Warningf("Failed to delete container root directory %q, err: %v", c.Root, err)
	}

	// "If any poststop hook fails, the runtime MUST log a warning, but the
	// remaining hooks and lifecycle continue as if the hook had succeeded".
	if c.Spec.Hooks != nil && (c.Status == Created || c.Status == Running) {
		executeHooksBestEffort(c.Spec.Hooks.Poststop, c.State())
	}

	if err := os.RemoveAll(c.Root); err != nil {
		log.Warningf("Failed to delete container root directory %q, err: %v", c.Root, err)
	}

	// If we are the first container in the sandbox, take the sandbox down
	// as well.
	if c.Sandbox != nil && c.Sandbox.ID == c.ID {
		if err := c.Sandbox.Destroy(); err != nil {
			log.Warningf("Failed to destroy sandbox %q: %v", c.Sandbox.ID, err)
		}
	}

	c.Sandbox = nil
	c.Status = Stopped
	return nil
}

// save saves the container metadata to a file.
func (c *Container) save() error {
	log.Debugf("Save container %q", c.ID)
	if err := os.MkdirAll(c.Root, 0711); err != nil {
		return fmt.Errorf("error creating container root directory %q: %v", c.Root, err)
	}
	meta, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("error marshaling container metadata: %v", err)
	}
	metaFile := filepath.Join(c.Root, metadataFilename)
	if err := ioutil.WriteFile(metaFile, meta, 0640); err != nil {
		return fmt.Errorf("error writing container metadata: %v", err)
	}
	return nil
}

// exists returns true if the given file exists.
func exists(f string) bool {
	if _, err := os.Stat(f); err == nil {
		return true
	} else if !os.IsNotExist(err) {
		log.Warningf("error checking for file %q: %v", f, err)
	}
	return false
}
