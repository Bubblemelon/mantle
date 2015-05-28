// Copyright 2015 CoreOS, Inc.
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

package kola

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coreos/mantle/platform"
)

// NativeRunner is a closure passed to all kola test functions and used
// to run native go functions directly on kola machines. It is necessary
// glue until kola does introspection.
type NativeRunner func(funcName string, m platform.Machine) error

type Test struct {
	Name        string // should be uppercase and unique
	Run         func(platform.TestCluster) error
	NativeFuncs map[string]func() error
	CloudConfig string
	ClusterSize int
	Platforms   []string // whitelist of platforms to run test against -- defaults to all
}

// maps names to tests
var Tests = map[string]*Test{}

// panic if existing name is registered
func Register(t *Test) {
	_, ok := Tests[t.Name]
	if ok {
		panic("test already registered with same name")
	}
	Tests[t.Name] = t
}

// test runner and kola entry point
func RunTests(args []string) int {
	if len(args) > 1 {
		fmt.Fprintf(os.Stderr, "Extra arguements specified. Usage: 'kola run [glob pattern]'\n")
		return 2
	}
	var pattern string
	if len(args) == 1 {
		pattern = args[0]
	} else {
		pattern = "*" // run all tests by default
	}

	var ranTests int //count successful tests
	for _, t := range Tests {
		match, err := filepath.Match(pattern, t.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		if !match {
			continue
		}

		// run all platforms if whitelist is nil
		if t.Platforms == nil {
			t.Platforms = []string{"qemu", "gce"}
		}

		for _, pltfrm := range t.Platforms {
			err := runTest(t, pltfrm)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v failed on %v: %v\n", t.Name, pltfrm, err)
				return 1
			}
			fmt.Printf("test %v ran successfully on %v\n", t.Name, pltfrm)
			ranTests++
		}
	}
	fmt.Fprintf(os.Stderr, "All %v test(s) ran successfully!\n", ranTests)
	return 0
}

// create a cluster and run test
func runTest(t *Test, pltfrm string) error {
	var err error
	var cluster platform.Cluster

	if pltfrm == "qemu" {
		cluster, err = platform.NewQemuCluster(*QemuImage)
	} else if pltfrm == "gce" {
		cluster, err = platform.NewGCECluster(GCEOpts())
	} else {
		fmt.Fprintf(os.Stderr, "Invalid platform: %v", pltfrm)
	}

	if err != nil {
		return fmt.Errorf("Cluster failed: %v", err)
	}
	defer func() {
		if err := cluster.Destroy(); err != nil {
			fmt.Fprintf(os.Stderr, "cluster.Destroy(): %v\n", err)
		}
	}()

	url, err := cluster.GetDiscoveryURL(t.ClusterSize)
	if err != nil {
		return fmt.Errorf("Failed to create discovery endpoint: %v", err)
	}

	cfgs := makeConfigs(url, t.CloudConfig, t.ClusterSize)

	for i := 0; i < t.ClusterSize; i++ {
		_, err := cluster.NewMachine(cfgs[i])
		if err != nil {
			return fmt.Errorf("Cluster failed starting machine: %v", err)
		}
		fmt.Fprintf(os.Stderr, "%v instance up\n", pltfrm)
	}

	// drop kolet binary on machines
	if t.NativeFuncs != nil {
		for _, m := range cluster.Machines() {
			err = scpFile(m, "./kolet") //TODO pb: locate local binary path with `which` once kolet is in overlay
			if err != nil {
				return fmt.Errorf("dropping kolet binary: %v", err)
			}
		}
	}
	// Cluster -> TestCluster
	tcluster := platform.TestCluster{t.Name, cluster}

	// run test
	err = t.Run(tcluster)
	return err
}

// scpFile copies file from src path to ~/ on machine
func scpFile(m platform.Machine, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	session, err := m.SSHSession()
	if err != nil {
		return fmt.Errorf("Error establishing ssh session: %v", err)
	}
	defer session.Close()

	// machine reads file from stdin
	session.Stdin = in

	// cat file to fs
	_, filename := filepath.Split(src)
	_, err = session.CombinedOutput(fmt.Sprintf("install -m 0755 /dev/stdin ./%s", filename))
	if err != nil {
		return err
	}
	return nil
}

// replaces $discovery with discover url in etcd cloud config and
// replaces $name with a unique name
func makeConfigs(url, cfg string, csize int) []string {
	cfg = strings.Replace(cfg, "$discovery", url, -1)

	var cfgs []string
	for i := 0; i < csize; i++ {
		cfgs = append(cfgs, strings.Replace(cfg, "$name", "instance"+strconv.Itoa(i), -1))
	}
	return cfgs
}