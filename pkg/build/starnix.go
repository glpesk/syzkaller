// Copyright 2024 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package build

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/syzkaller/pkg/osutil"
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
	imageFilePath := filepath.Join(params.OutputDir, "image")
	imageFile, err := os.Create(imageFilePath)
	if err != nil {
		return ImageDetails{}, fmt.Errorf("failed to create output file: %w", err)
	}
	defer imageFile.Close()

	if _, err := runSandboxed(time.Hour*2, params.KernelDir, "scripts/fx", "build"); err != nil {
		return ImageDetails{}, err
	}
	ffxBinary, err := getToolPath(params.KernelDir, "ffx")
	if err != nil {
		return ImageDetails{}, err
	}
	productBundlePathRaw, err := runSandboxed(
		30*time.Second,
		params.KernelDir,
		ffxBinary,
		"config", "get", "product.path",
	)
	if err != nil {
		return ImageDetails{}, nil
	}
	productBundlePath := strings.Trim(string(productBundlePathRaw), "\"")
	fxfsPath, err := runSandboxed(
		30*time.Second,
		params.KernelDir,
		ffxBinary,
		"product", "get-image-path", productBundlePath,
		"--slot", "a",
		"--image-type", "fxfs",
	)
	if err != nil {
		return ImageDetails{}, nil
	}
	if err := osutil.CopyFile(string(fxfsPath), imageFilePath); err != nil {
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

// Get the currently-selected build dir in a Fuchsia checkout.
func getFuchsiaBuildDir(fuchsiaDir string) (string, error) {
	fxBuildDir := filepath.Join(fuchsiaDir, ".fx-build-dir")
	contents, err := os.ReadFile(fxBuildDir)
	if err != nil {
		return "", fmt.Errorf("failed to read %q: %w", fxBuildDir, err)
	}

	buildDir := strings.TrimSpace(string(contents))
	if !filepath.IsAbs(buildDir) {
		buildDir = filepath.Join(fuchsiaDir, buildDir)
	}

	return buildDir, nil
}

// Subset of data format used in tool_paths.json.
type toolMetadata struct {
	Name string
	Path string
}

// Resolve a tool by name using tool_paths.json in the build dir.
func getToolPath(fuchsiaDir, toolName string) (string, error) {
	buildDir, err := getFuchsiaBuildDir(fuchsiaDir)
	if err != nil {
		return "", err
	}

	jsonPath := filepath.Join(buildDir, "tool_paths.json")
	jsonBlob, err := os.ReadFile(jsonPath)
	if err != nil {
		return "", fmt.Errorf("failed to read %q: %w", jsonPath, err)
	}
	var metadataList []toolMetadata
	if err := json.Unmarshal(jsonBlob, &metadataList); err != nil {
		return "", fmt.Errorf("failed to parse %q: %w", jsonPath, err)
	}

	for _, metadata := range metadataList {
		if metadata.Name == toolName {
			return filepath.Join(buildDir, metadata.Path), nil
		}
	}

	return "", fmt.Errorf("no path found for tool %q in %q", toolName, jsonPath)
}
