// Copyright 2022 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/tool"
	"github.com/google/syzkaller/vm/qemu"

	"github.com/google/syzkaller/pkg/mgrconfig"
)

func checkDir(name string, path string) {
	if !osutil.IsExist(path) {
		tool.Failf("%s arg \"%s\" is not a directory", name, path)
	}
}

func runCmd(cmd exec.Cmd) {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

func addSshKeys(fuchsia *string) {
	zbi := exec.Command(
		filepath.Join(*fuchsia, "out/x64/host_x64/zbi"),
		"-o",
		filepath.Join(*fuchsia, "out/x64/fuchsia-ssh.zbi"),
		filepath.Join(*fuchsia, "out/x64/fuchsia.zbi"),
		"--entry",
		fmt.Sprintf(
			"data/ssh/authorized_keys=%s",
			filepath.Join(*fuchsia, ".ssh/authorized_keys"),
		),
	)
	runCmd(*zbi)

	cpPath, err := exec.LookPath("cp")
	if err != nil {
		tool.Fail(err)
	}
	cpZbi := exec.Command(
		cpPath,
		filepath.Join(*fuchsia, "out/x64/fuchsia-ssh.zbi"),
		filepath.Join(*fuchsia, "out/x64/syzdeps/fuchsia-ssh.zbi"),
	)
	runCmd(*cpZbi)
}

func extendFvm(fuchsia *string) {
	cpPath, err := exec.LookPath("cp")
	if err != nil {
		tool.Fail(err)
	}
	cpFvm := exec.Command(
		cpPath,
		filepath.Join(*fuchsia, "out/x64/obj/build/images/fuchsia/fuchsia/fvm.blk"),
		filepath.Join(*fuchsia, "out/x64/syzdeps/fvm-extended.blk"),
	)
	runCmd(*cpFvm)

	extendFvm := exec.Command(
		filepath.Join(*fuchsia, "out/x64/host_x64/fvm"),
		filepath.Join(*fuchsia, "out/x64/syzdeps/fvm-extended.blk"),
		"extend",
		"--length", "3G",
	)
	runCmd(*extendFvm)
}

func build(fuchsia *string, syzkaller *string, noSet bool) {
	if err := os.Chdir(*fuchsia); err != nil {
		tool.Fail(err)
	}

	fx := filepath.Join(*fuchsia, "scripts/fx")

	if noSet {
		fmt.Println("Skipping `fx set`...")
	} else {
		fmt.Println("Running `fx set`...")
		fxSet := exec.Command(
			fx,
			"--dir", "out/x64",
			"set", "core.x64",
			"--with-base", "//bundles/tools",
			"--with-base", "//src/testing/fuzzing/syzkaller",
			fmt.Sprintf("--args=syzkaller_dir=\"%s\"", *syzkaller),
			"--variant=kasan",
		)
		runCmd(*fxSet)
	}

	fmt.Println("\nRunning `fx build`...")
	fxBuild := exec.Command(fx, "build")
	runCmd(*fxBuild)

	if err := os.Chdir(*syzkaller); err != nil {
		tool.Fail(err)
	}

	fmt.Println("\nAdding ssh keys...")
	addSshKeys(fuchsia)

	fmt.Println("\nCopying and extending fvm...")
	extendFvm(fuchsia)

	fmt.Println("\nBuilding syzkaller for fuchsia...")
	makePath, err := exec.LookPath("make")
	if err != nil {
		tool.Fail(err)
	}
	makeSyz := exec.Command(
		makePath,
		"TARGETOS=fuchsia",
		"TARGETARCH=amd64",
		fmt.Sprintf("SOURCEDIR=%s", *fuchsia),
	)
	runCmd(*makeSyz)
}

func makeConfig(fuchsia *string, syzkaller *string, workdir *string, filename *string) {
	vm := qemu.Config{
		Count:    10,
		Qemu:     "qemu-system-x86_64",
		CPU:      4,
		Mem:      2048,
		Kernel:   filepath.Join(*fuchsia, "out/x64/multiboot.bin"),
		Initrd:   filepath.Join(*fuchsia, "out/x64/syzdeps/fuchsia-ssh.zbi"),
		Snapshot: true,
	}
	vmJson, err := json.Marshal(vm)
	if err != nil {
		tool.Fail(err)
	}
	cfg := mgrconfig.Config{
		Name:      "fuchsia",
		RawTarget: "fuchsia/amd64",
		HTTP:      ":12345",
		Workdir:   *workdir,
		KernelObj: filepath.Join(*fuchsia, "out/x64/kernel_x64-kasan/obj/zircon/kernel"),
		Syzkaller: *syzkaller,
		Image:     filepath.Join(*fuchsia, "out/x64/syzdeps/fvm-extended.blk"),
		SSHKey:    filepath.Join(*fuchsia, ".ssh/pkey"),
		Reproduce: false,
		Cover:     false,
		Procs:     8,
		Sandbox:   "none",
		Type:      "qemu",
		VM:        vmJson,
	}
	cfgJson, err := json.Marshal(cfg)
	if err != nil {
		tool.Fail(err)
	}
	cfgPath := filepath.Join(*workdir, *filename)
	f, err := os.Create(cfgPath)
	if err != nil {
		tool.Fail(err)
	}
	if _, err := f.Write(cfgJson); err != nil {
		tool.Fail(err)
	}
	f.Close()
	fmt.Println("Wrote a basic syz-manager config file to ", cfgPath)
}

func run(fuchsia *string, syzkaller *string, config *string) {
	if err := os.Chdir(*syzkaller); err != nil {
		tool.Fail(err)
	}
	os.Setenv("PATH",
		fmt.Sprintf("%s:%s:%s",
			os.Getenv("PATH"),
			filepath.Join(
				*fuchsia,
				"prebuilt/third_party/qemu/linux-x64/bin",
			),
			filepath.Join(
				*fuchsia,
				"prebuilt/third_party/qemu/mac-x64/bin",
			),
		),
	)

	syzMgrPath, err := exec.LookPath("bin/syz-manager")
	if err != nil {
		tool.Fail(err)
	}
	runSyzMgr := exec.Command(
		syzMgrPath,
		"-config", *config,
	)
	runCmd(*runSyzMgr)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("options:\nbuild\nmake_config\nrun\n")
		tool.Failf("must provide an option")
	}

	buildCommand := flag.NewFlagSet("build", flag.ExitOnError)
	buildFuchsia := buildCommand.String("fuchsia", "", "")
	buildSyzkaller := buildCommand.String("syzkaller", "", "")
	buildNoSet := buildCommand.Bool("no_set", false, "")

	makeConfigCommand := flag.NewFlagSet("make_config", flag.ExitOnError)
	makeConfigFuchsia := makeConfigCommand.String("fuchsia", "", "")
	makeConfigSyzkaller := makeConfigCommand.String("syzkaller", "", "")
	makeConfigWorkdir := makeConfigCommand.String("workdir", "", "")
	makeConfigFile := makeConfigCommand.String("filename", "", "")

	runCommand := flag.NewFlagSet("run", flag.ExitOnError)
	runFuchsia := runCommand.String("fuchsia", "", "")
	runSyzkaller := runCommand.String("syzkaller", "", "")
	runConfig := runCommand.String("config", "", "")

	switch os.Args[1] {
	case "build":
		buildCommand.Parse(os.Args[2:])
		checkDir("fuchsia", *buildFuchsia)
		checkDir("syzkaller", *buildSyzkaller)
		build(buildFuchsia, buildSyzkaller, *buildNoSet)
	case "make_config":
		makeConfigCommand.Parse(os.Args[2:])
		checkDir("fuchsia", *makeConfigFuchsia)
		checkDir("syzkaller", *makeConfigSyzkaller)
		checkDir("workdir", *makeConfigWorkdir)
		makeConfig(makeConfigFuchsia, makeConfigSyzkaller, makeConfigWorkdir, makeConfigFile)
	case "run":
		runCommand.Parse(os.Args[2:])
		checkDir("fuchsia", *runFuchsia)
		checkDir("syzkaller", *runSyzkaller)
		checkDir("config", *runConfig)
		run(runFuchsia, runSyzkaller, runConfig)
	default:
		flag.PrintDefaults()
		tool.Failf("invalid option %s", os.Args[1])
	}
}
