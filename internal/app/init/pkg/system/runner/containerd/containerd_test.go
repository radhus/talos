/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package containerd_test

import (
	"bytes"
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/suite"
	"github.com/talos-systems/talos/internal/app/init/pkg/system/runner"
	containerdrunner "github.com/talos-systems/talos/internal/app/init/pkg/system/runner/containerd"
	"github.com/talos-systems/talos/internal/app/init/pkg/system/runner/process"
	"github.com/talos-systems/talos/internal/pkg/constants"
	"github.com/talos-systems/talos/pkg/userdata"
)

const (
	containerdNamespace = "talostest"
	busyboxImage        = "docker.io/library/busybox:latest"
)

type ContainerdSuite struct {
	suite.Suite

	tmpDir string

	containerdRunner runner.Runner
	containerdWg     sync.WaitGroup

	client *containerd.Client
	image  containerd.Image
}

func (suite *ContainerdSuite) SetupSuite() {
	var err error

	args := &runner.Args{
		ID:          "containerd",
		ProcessArgs: []string{"/rootfs/bin/containerd"},
	}

	suite.tmpDir, err = ioutil.TempDir("", "talos")
	suite.Require().NoError(err)

	suite.containerdRunner = process.NewRunner(
		&userdata.UserData{},
		args,
		runner.WithType(runner.Once),
		runner.WithLogPath(suite.tmpDir),
		runner.WithEnv([]string{"PATH=/rootfs/bin:" + constants.PATH}),
	)
	suite.containerdWg.Add(1)
	go func() {
		defer suite.containerdWg.Done()
		suite.Require().NoError(suite.containerdRunner.Run())
	}()

	suite.client, err = containerd.New(constants.ContainerdAddress)
	suite.Require().NoError(err)

	ctx := namespaces.WithNamespace(context.Background(), containerdNamespace)

	suite.image, err = suite.client.Pull(ctx, busyboxImage, containerd.WithPullUnpack)
	suite.Require().NoError(err)
}

func (suite *ContainerdSuite) TearDownSuite() {
	suite.Require().NoError(suite.client.Close())

	suite.Require().NoError(suite.containerdRunner.Stop())
	suite.containerdWg.Wait()

	suite.Require().NoError(os.RemoveAll(suite.tmpDir))
}

func (suite *ContainerdSuite) TestRunSuccess() {
	r := containerdrunner.NewRunner(&userdata.UserData{}, &runner.Args{
		ID:          "test",
		ProcessArgs: []string{"/bin/sh", "-c", "exit 0"},
	},
		runner.WithType(runner.Once),
		runner.WithLogPath(suite.tmpDir),
		runner.WithNamespace(containerdNamespace),
		runner.WithContainerImage(busyboxImage),
	)

	suite.Assert().NoError(r.Run())
	// calling stop when Run has finished is no-op
	suite.Assert().NoError(r.Stop())
}

func (suite *ContainerdSuite) TestRunTwice() {
	// running same container twice should be fine
	// (checks that containerd state is cleaned up properly)
	for i := 0; i < 2; i++ {
		r := containerdrunner.NewRunner(&userdata.UserData{}, &runner.Args{
			ID:          "runtwice",
			ProcessArgs: []string{"/bin/sh", "-c", "exit 0"},
		},
			runner.WithType(runner.Once),
			runner.WithLogPath(suite.tmpDir),
			runner.WithNamespace(containerdNamespace),
			runner.WithContainerImage(busyboxImage),
		)

		suite.Assert().NoError(r.Run())
		// calling stop when Run has finished is no-op
		suite.Assert().NoError(r.Stop())
	}
}

func (suite *ContainerdSuite) TestRunLogs() {
	r := containerdrunner.NewRunner(&userdata.UserData{}, &runner.Args{
		ID:          "logtest",
		ProcessArgs: []string{"/bin/sh", "-c", "echo -n \"Test 1\nTest 2\n\""},
	},
		runner.WithType(runner.Once),
		runner.WithLogPath(suite.tmpDir),
		runner.WithNamespace(containerdNamespace),
		runner.WithContainerImage(busyboxImage),
	)

	suite.Assert().NoError(r.Run())

	logFile, err := os.Open(filepath.Join(suite.tmpDir, "logtest.log"))
	suite.Assert().NoError(err)

	// nolint: errcheck
	defer logFile.Close()

	logContents, err := ioutil.ReadAll(logFile)
	suite.Assert().NoError(err)

	suite.Assert().Equal([]byte("Test 1\nTest 2\n"), logContents)
}

func (suite *ContainerdSuite) TestStopFailingAndRestarting() {
	testDir := filepath.Join(suite.tmpDir, "test")
	suite.Assert().NoError(os.Mkdir(testDir, 0770))

	testFile := filepath.Join(testDir, "talos-test")
	// nolint: errcheck
	_ = os.Remove(testFile)

	r := containerdrunner.NewRunner(&userdata.UserData{}, &runner.Args{
		ID:          "endless",
		ProcessArgs: []string{"/bin/sh", "-c", "test -f " + testFile + " && echo ok || (echo fail; false)"},
	},
		runner.WithType(runner.Forever),
		runner.WithLogPath(suite.tmpDir),
		runner.WithRestartInterval(5*time.Millisecond),
		runner.WithNamespace(containerdNamespace),
		runner.WithContainerImage(busyboxImage),
		runner.WithOCISpecOpts(
			oci.WithMounts([]specs.Mount{
				{Type: "bind", Destination: testDir, Source: testDir, Options: []string{"bind", "ro"}},
			}),
		),
	)

	done := make(chan error, 1)

	go func() {
		done <- r.Run()
	}()

	time.Sleep(500 * time.Millisecond)

	select {
	case err := <-done:
		suite.Assert().Failf("task should be running", "error: %s", err)
		return
	default:
	}

	f, err := os.Create(testFile)
	suite.Assert().NoError(err)
	suite.Assert().NoError(f.Close())

	time.Sleep(500 * time.Millisecond)

	select {
	case err = <-done:
		suite.Assert().Failf("task should be running", "error: %s", err)
		return
	default:
	}

	suite.Assert().NoError(r.Stop())
	<-done

	logFile, err := os.Open(filepath.Join(suite.tmpDir, "endless.log"))
	suite.Assert().NoError(err)

	// nolint: errcheck
	defer logFile.Close()

	logContents, err := ioutil.ReadAll(logFile)
	suite.Assert().NoError(err)

	suite.Assert().Truef(bytes.Contains(logContents, []byte("ok\n")), "logContents doesn't contain success entry: %v", logContents)
	suite.Assert().Truef(bytes.Contains(logContents, []byte("fail\n")), "logContents doesn't contain fail entry: %v", logContents)
}

func (suite *ContainerdSuite) TestStopSigKill() {
	r := containerdrunner.NewRunner(&userdata.UserData{}, &runner.Args{
		ID:          "nokill",
		ProcessArgs: []string{"/bin/sh", "-c", "trap -- '' SIGTERM; while true; do sleep 1; done"},
	},
		runner.WithType(runner.Forever),
		runner.WithLogPath(suite.tmpDir),
		runner.WithNamespace(containerdNamespace),
		runner.WithContainerImage(busyboxImage),
		runner.WithRestartInterval(5*time.Millisecond),
		runner.WithGracefulShutdownTimeout(10*time.Millisecond))

	done := make(chan error, 1)

	go func() {
		done <- r.Run()
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		suite.Assert().Fail("container should be still running")
	default:
	}

	time.Sleep(100 * time.Millisecond)

	suite.Assert().NoError(r.Stop())
	<-done
}

func (suite *ContainerdSuite) TestImportSuccess() {
	reqs := []*containerdrunner.ImportRequest{
		{
			Path: "/rootfs/usr/images/osd.tar",
			Options: []containerd.ImportOpt{
				containerd.WithIndexName("testtalos/osd"),
			},
		},
		{
			Path: "/rootfs/usr/images/proxyd.tar",
			Options: []containerd.ImportOpt{
				containerd.WithIndexName("testtalos/proxyd"),
			},
		},
	}
	suite.Assert().NoError(containerdrunner.Import(containerdNamespace, reqs...))

	ctx := namespaces.WithNamespace(context.Background(), containerdNamespace)
	for _, imageName := range []string{"testtalos/osd", "testtalos/proxyd"} {
		image, err := suite.client.ImageService().Get(ctx, imageName)
		suite.Require().NoError(err)
		suite.Require().Equal(imageName, image.Name)
	}
}

func (suite *ContainerdSuite) TestImportFail() {
	reqs := []*containerdrunner.ImportRequest{
		{
			Path: "/rootfs/usr/images/osd.tar",
			Options: []containerd.ImportOpt{
				containerd.WithIndexName("testtalos/osd2"),
			},
		},
		{
			Path: "/rootfs/usr/images/nothere.tar",
			Options: []containerd.ImportOpt{
				containerd.WithIndexName("testtalos/nothere"),
			},
		},
	}
	suite.Assert().Error(containerdrunner.Import(containerdNamespace, reqs...))
}

func TestContainerdSuite(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("can't run the test as non-root")
	}
	_, err := os.Stat("/rootfs/bin/containerd")
	if err != nil {
		t.Skip("containerd binary is not available, skipping the test")
	}

	suite.Run(t, new(ContainerdSuite))
}
