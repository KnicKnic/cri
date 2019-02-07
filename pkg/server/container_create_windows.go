// +build windows

/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/oci"
	runhcsoptions "github.com/containerd/containerd/runtime/v2/runhcs/options"
	"github.com/containerd/typeurl"
	"github.com/davecgh/go-spew/spew"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"

	"github.com/containerd/cri/pkg/annotations"
	customopts "github.com/containerd/cri/pkg/containerd/opts"
	ctrdutil "github.com/containerd/cri/pkg/containerd/util"
	cio "github.com/containerd/cri/pkg/server/io"
	containerstore "github.com/containerd/cri/pkg/store/container"
	"github.com/containerd/cri/pkg/util"

	"github.com/golang/protobuf/jsonpb"
)

func init() {
	typeurl.Register(&containerstore.Metadata{},
		"github.com/containerd/cri/pkg/store/container", "Metadata")
}

// CreateContainer creates a new container in the given PodSandbox.
func (c *criService) CreateContainer(ctx context.Context, r *runtime.CreateContainerRequest) (_ *runtime.CreateContainerResponse, retErr error) {
	config := r.GetConfig()
	sandboxConfig := r.GetSandboxConfig()
	sandbox, err := c.sandboxStore.Get(r.GetPodSandboxId())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find sandbox id %q", r.GetPodSandboxId())
	}
	sandboxID := sandbox.ID
	s, err := sandbox.Container.Task(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get sandbox container task")
	}
	sandboxPid := s.Pid()

	// Generate unique id and name for the container and reserve the name.
	// Reserve the container name to avoid concurrent `CreateContainer` request creating
	// the same container.
	id := util.GenerateID()
	metadata := config.GetMetadata()
	if metadata == nil {
		return nil, errors.New("container config must include metadata")
	}
	name := makeContainerName(metadata, sandboxConfig.GetMetadata())
	logrus.Debugf("Generated id %q for container %q", id, name)
	if err = c.containerNameIndex.Reserve(name, id); err != nil {
		return nil, errors.Wrapf(err, "failed to reserve container name %q", name)
	}
	defer func() {
		// Release the name if the function returns with an error.
		if retErr != nil {
			c.containerNameIndex.ReleaseByName(name)
		}
	}()

	// Create initial internal container metadata.
	meta := containerstore.Metadata{
		ID:        id,
		Name:      name,
		SandboxID: sandboxID,
		Config:    config,
	}

	// Prepare container image snapshot. For container, the image should have
	// been pulled before creating the container, so do not ensure the image.
	image, err := c.localResolve(config.GetImage().GetImage())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve image %q", config.GetImage().GetImage())
	}

	// Run container using the same runtime with sandbox.
	sandboxInfo, err := sandbox.Container.Info(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get sandbox %q info", sandboxID)
	}

	// Create container root directory.
	containerRootDir := c.getContainerRootDir(id)
	if err = c.os.MkdirAll(containerRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create container root directory %q",
			containerRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the container root directory.
			if err = c.os.RemoveAll(containerRootDir); err != nil {
				logrus.WithError(err).Errorf("Failed to remove container root directory %q",
					containerRootDir)
			}
		}
	}()
	volatileContainerRootDir := c.getVolatileContainerRootDir(id)
	if err = c.os.MkdirAll(volatileContainerRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create volatile container root directory %q",
			volatileContainerRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the volatile container root directory.
			if err = c.os.RemoveAll(volatileContainerRootDir); err != nil {
				logrus.WithError(err).Errorf("Failed to remove volatile container root directory %q",
					volatileContainerRootDir)
			}
		}
	}()

	var sandboxPlatform string
	
	// Get the RuntimeHandler config overrides
	ociRuntime := c.config.DefaultRuntime
	if sandbox.RuntimeHandler != "" {
		ociRuntime = c.config.Runtimes[sandbox.RuntimeHandler]
	}
	runtimeOpts, err := generateRuntimeOptions(ociRuntime, c.config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate runtime options")
	}
	rhcso := runtimeOpts.(*runhcsoptions.Options)
	sandboxPlatform = rhcso.SandboxPlatform
	if sandboxPlatform == "" {
		sandboxPlatform = "windows/amd64"
	}

	spec, err := c.generateContainerSpec(id, sandboxID, sandboxPid, sandbox.NetNSPath, config, sandboxConfig, sandboxPlatform, &image.ImageSpec.Config)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate container %q spec", id)
	}

	logrus.Debugf("Container %q spec: %#+v", id, spew.NewFormatter(spec))

	// Set snapshotter before any other options.
	opts := []containerd.NewContainerOpts{
		containerd.WithSnapshotter(c.getDefaultSnapshotterForPlatform(sandboxPlatform)),
		customopts.WithNewSnapshot(id, image.Image),
	}

	meta.ImageRef = image.ID
	meta.StopSignal = image.ImageSpec.Config.StopSignal

	// Get container log path.
	if config.GetLogPath() != "" {
		meta.LogPath = filepath.Join(sandbox.Config.GetLogDirectory(), config.GetLogPath())
	}

	containerIO, err := cio.NewContainerIO(id,
		cio.WithNewFIFOs(volatileContainerRootDir, config.GetTty(), config.GetStdin()))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create container io")
	}
	defer func() {
		if retErr != nil {
			if err := containerIO.Close(); err != nil {
				logrus.WithError(err).Errorf("Failed to close container io %q", id)
			}
		}
	}()

	containerLabels := buildLabels(config.Labels, containerKindContainer)
	runtimeOptions, err := getRuntimeOptions(sandboxInfo)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get runtime options")
	}
	opts = append(opts,
		containerd.WithSpec(spec),
		containerd.WithContainerLabels(containerLabels),
		containerd.WithContainerExtension(containerMetadataExtension, &meta),
		containerd.WithRuntime(sandboxInfo.Runtime.Name, runtimeOptions))

	var cntr containerd.Container
	if cntr, err = c.client.NewContainer(ctx, id, opts...); err != nil {
		return nil, errors.Wrap(err, "failed to create containerd container")
	}
	defer func() {
		if retErr != nil {
			deferCtx, deferCancel := ctrdutil.DeferContext()
			defer deferCancel()
			if err := cntr.Delete(deferCtx, containerd.WithSnapshotCleanup); err != nil {
				logrus.WithError(err).Errorf("Failed to delete containerd container %q", id)
			}
		}
	}()

	status := containerstore.Status{CreatedAt: time.Now().UnixNano()}
	container, err := containerstore.NewContainer(meta,
		containerstore.WithStatus(status, containerRootDir),
		containerstore.WithContainer(cntr),
		containerstore.WithContainerIO(containerIO),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create internal container object for %q", id)
	}
	defer func() {
		if retErr != nil {
			// Cleanup container checkpoint on error.
			if err := container.Delete(); err != nil {
				logrus.WithError(err).Errorf("Failed to cleanup container checkpoint for %q", id)
			}
		}
	}()

	// Add container into container store.
	if err := c.containerStore.Add(container); err != nil {
		return nil, errors.Wrapf(err, "failed to add container %q into store", id)
	}

	return &runtime.CreateContainerResponse{ContainerId: id}, nil
}

func (c *criService) generateContainerSpec(id string, sandboxID string, sandboxPid uint32, netnsPath string, config *runtime.ContainerConfig,
	sandboxConfig *runtime.PodSandboxConfig, sandboxPlatform string, imageConfig *imagespec.ImageConfig) (*runtimespec.Spec, error) {
	// Creates a spec Generator with the default spec.
	ctx := ctrdutil.NamespacedContext()
	spec, err := oci.GenerateSpecWithPlatform(ctx, nil, sandboxPlatform, &containers.Container{ID: id})
	if err != nil {
		return nil, err
	}
	g := newSpecGenerator(spec)

	if err := setOCIProcessArgs(&g, config, imageConfig); err != nil {
		return nil, err
	}

	if config.GetWorkingDir() != "" {
		g.SetProcessCwd(config.GetWorkingDir())
	} else if imageConfig.WorkingDir != "" {
		g.SetProcessCwd(imageConfig.WorkingDir)
	}

	g.SetProcessTerminal(config.GetTty())

	// Apply envs from image config first, so that envs from container config
	// can override them.
	if err := addImageEnvs(&g, imageConfig.Env); err != nil {
		return nil, err
	}
	for _, e := range config.GetEnvs() {
		g.AddProcessEnv(e.GetKey(), e.GetValue())
	}

	// Clear the root location since runhcs sets it on the mount path in the
	// guest.
	g.Config.Root = nil

	// Set the Network Namespace
	g.SetWindowsNetworkNamespace(netnsPath)

	g.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeContainer)
	g.AddAnnotation(annotations.SandboxID, sandboxID)

	m := jsonpb.Marshaler{}
	configJson, _ := m.MarshalToString(config)
	sconfigJson, _ := m.MarshalToString(sandboxConfig)
	logrus.Debugf("configs eoa;fdno;s container: %s, pod: %s", configJson, sconfigJson)

	// Add OCI Mounts
	for _, m := range config.GetMounts() {

		//normalize the format of the host and container path
		formattedSource, err := filepath.EvalSymlinks(strings.Replace(m.HostPath, "/", "\\", -1))
		if err != nil {
			return nil, err
		}
		var formattedDestination string
		if sandboxPlatform == "linux/amd64" {
			formattedDestination = strings.Replace(m.ContainerPath, "\\", "/", -1)
			//kubelet will prepend c: if it's running on Windows and there's no drive letter, so we need to strip it out
			if match, _ := regexp.MatchString("^[A-Za-z]:", formattedDestination); match {
				formattedDestination = formattedDestination[2:]
			}
		} else {
			formattedDestination = strings.Replace(m.ContainerPath, "/", "\\", -1)
		}

		mo := runtimespec.Mount{
			Source:      formattedSource,
			Destination: formattedDestination,
			Options:     []string{"ro"},
		}
		if !m.Readonly {
			mo.Options[0] = "rw"
		}
		if strings.HasPrefix(formattedSource, `\\.\PHYSICALDRIVE`) {
			mo.Type = "physical-disk"
		} else if strings.HasPrefix(formattedSource, `\\.\pipe`) {
			// mo.Type == "" for pipe but we don't want to Stat the path.
			if sandboxPlatform == "linux/amd64" {
				return nil, errors.Errorf(`pipe mount.HostPath '%s' not supported for LCOW`, formattedSource)
			}
		} else {
			s, err := os.Stat(formattedSource)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to Stat mount.HostPath '%s'", formattedSource)
			}
			if !s.IsDir() {
				ext := strings.ToLower(filepath.Ext(formattedSource))
				if ext == ".vhd" || ext == ".vhdx" {
					mo.Type = "virtual-disk"
					mo.Options = append(mo.Options, "bind")
				}
			}
			if sandboxPlatform == "linux/amd64" {
				switch mo.Type {
				case "":
					// Linux requires a folder/file to be bind mount.
					mo.Type = "bind"
					mo.Options = append(mo.Options, "rbind")
				case "virtual-disk", "physical-disk":
					mo.Options = append(mo.Options, "bind")

				}
			}
		}

		// TODO: JTERRY75 - Mount Propagation for LCOW?

		g.AddMount(mo)
	}

	return g.Config, nil
}
