/*
Copyright 2018 The Kubernetes Authors.

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

package remote

import (
	"fmt"
	"os"

	compute "google.golang.org/api/compute/v1"
	"k8s.io/klog"
)

const (
	archiveName = "e2e_driver_binaries.tar.gz"
)

// TestContext holds the CSI Client handle to a remotely connected Driver
// as well as a handle to the Instance that the driver is running on
type TestContext struct {
	Instance *InstanceInfo
	Client   *CsiClient
	proc     *processes
}

// ClientConfig contains all the parameters required to package a new
// driver and run it remotely on a GCE Instance
type ClientConfig struct {
	// Absolute path of the package
	PkgPath string
	// Absolute path of the driver binary to copy remotely
	BinPath string
	// Path on remote instance workspace
	WorkspaceDir string
	// Command to run on remote instance to start the driver
	RunDriverCmd string
	// Port to use as SSH tunnel on both remote and local side.
	Port string
}

type processes struct {
	sshTunnel    int
	remoteDriver int
}

// SetupInstance sets up the specified GCE Instance for E2E testing and returns a handle to the instance object for future use.
func SetupInstance(instanceProject, instanceZone, instanceName, instanceServiceAccount string, cs *compute.Service) (*InstanceInfo, error) {
	// Create the instance in the requisite zone
	instance, err := CreateInstanceInfo(instanceProject, instanceZone, instanceName, cs)
	if err != nil {
		return nil, err
	}

	err = instance.CreateOrGetInstance(instanceServiceAccount)
	if err != nil {
		return nil, err
	}
	return instance, nil
}

// SetupNewDriverAndClient gets the driver binary, runs it on the provided instance and connects
// a CSI client to it through SSH tunnelling. It returns a TestContext with both a handle to the instance
// that the driver is on and the CSI Client object to make CSI calls to the remote driver.
func SetupNewDriverAndClient(instance *InstanceInfo, config *ClientConfig) (*TestContext, error) {
	archivePath, err := CreateDriverArchive(archiveName, config.PkgPath, config.BinPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = os.Remove(archivePath)
		if err != nil {
			klog.Warningf("Failed to remove archive file %s: %v", archivePath, err)
		}
	}()

	// Upload archive to instance and run binaries
	driverPID, err := instance.UploadAndRun(archivePath, config.WorkspaceDir, config.RunDriverCmd)
	if err != nil {
		return nil, err
	}

	// Create an SSH tunnel from port to port
	sshPID, err := instance.CreateSSHTunnel(config.Port, config.Port)
	if err != nil {
		return nil, fmt.Errorf("SSH Tunnel pid %v encountered error: %v", sshPID, err)
	}

	client := CreateCSIClient(fmt.Sprintf("localhost:%s", config.Port))
	err = client.AssertCSIConnection()
	if err != nil {
		return nil, fmt.Errorf("asserting csi connection failed with: %v", err)
	}

	return &TestContext{
		Instance: instance,
		Client:   client,
		proc: &processes{
			sshTunnel:    sshPID,
			remoteDriver: driverPID,
		},
	}, nil
}

// TeardownDriverAndClient closes the CSI Client connection, closes the SSH tunnel
// Kills the driver process on the GCE instance, and cleans up the remote driver workspace
func TeardownDriverAndClient(context *TestContext) error {
	// Close the client connection
	err := context.Client.CloseConn()
	if err != nil {
		return fmt.Errorf("failed to close CSI Client connection: %v", err)
	}
	// Close the SSH tunnel
	proc, err := os.FindProcess(context.proc.sshTunnel)
	if err != nil {
		return fmt.Errorf("unable to efind process for ssh tunnel %v: %v", context.proc.sshTunnel, err)
	}
	if err = proc.Kill(); err != nil {
		return fmt.Errorf("failed to kill ssh tunnel process %v: %v", context.proc.sshTunnel, err)
	}

	// Kill the driver process on remote
	cmd := fmt.Sprintf("kill %v", context.proc.remoteDriver)
	output, err := context.Instance.SSH(cmd)
	if err != nil {
		return fmt.Errorf("failed to kill driver on remote instance, got output %s: %v", output, err)
	}

	// TODO: Cleanup driver workspace on remote

	return nil
}
