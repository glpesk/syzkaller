// Copyright 2024 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package build

import (
	"fmt"
	"time"

	"github.com/google/syzkaller/sys/targets"
)

type starnix struct{}

func (st starnix) build(params Params) (ImageDetails, error) {
	sysTarget := targets.Get(targets.Linux, params.TargetArch)
	arch := sysTarget.KernelArch
	if arch != "x86_64" {
		return ImageDetails{}, fmt.Errorf("unsupported starnix arch %v", arch)
	}
	arch = "x64"
	product := fmt.Sprintf("%s.%s", "workbench_eng", arch)
	if _, err := runSandboxed(
		time.Hour,
		params.KernelDir,
		"scripts/fx", "--dir", "out/"+arch,
		"set", product,
		"--with-base", "//src/testing/fuzzing/syzkaller/starnix:syzkaller_starnix",
	); err != nil {
		return ImageDetails{}, err
	}
	if _, err := runSandboxed(time.Hour*2, params.KernelDir, "scripts/fx", "build"); err != nil {
		return ImageDetails{}, err
	}
	return ImageDetails{}, nil
}

func (st starnix) clean(kernelDir, targetArch string) error {
	_, err := runSandboxed(
		time.Hour,
		kernelDir,
		"scripts/fx", "--dir", "out/"+targetArch,
		"clean",
	)
	return err
}
