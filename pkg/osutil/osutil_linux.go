// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package osutil

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// RemoveAll is similar to os.RemoveAll, but can handle more cases.
func RemoveAll(dir string) error {
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		name := filepath.Join(dir, f.Name())
		if f.IsDir() {
			RemoveAll(name)
		}
		unix.Unmount(name, unix.MNT_FORCE)
	}
	if err := os.RemoveAll(dir); err != nil {
		removeImmutable(dir)
		return os.RemoveAll(dir)
	}
	return nil
}

func SystemMemorySize() uint64 {
	var info syscall.Sysinfo_t
	syscall.Sysinfo(&info)
	return uint64(info.Totalram) // nolint:unconvert
}

func removeImmutable(fname string) error {
	// Reset FS_XFLAG_IMMUTABLE/FS_XFLAG_APPEND.
	fd, err := syscall.Open(fname, syscall.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, 0)
}

func Sandbox(cmd *exec.Cmd, user, net bool) error {
	enabled, uid, gid, homeDir, err := initSandbox()
	if err != nil || !enabled {
		return err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = new(syscall.SysProcAttr)
	}
	if net {
		cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWNET | syscall.CLONE_NEWIPC |
			syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID
	}
	if user {
		cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid: uid,
			Gid: gid,
		}
		if homeDir != "" {
			cmd.Env = append(os.Environ(), fmt.Sprintf("HOME=%s", homeDir))
		}
	}
	return nil
}

func SandboxChown(file string) error {
	enabled, uid, gid, _, err := initSandbox()
	if err != nil || !enabled {
		return err
	}
	return os.Chown(file, int(uid), int(gid))
}

var (
	sandboxOnce     sync.Once
	sandboxEnabled  = true
	sandboxUsername = "syzkaller"
	sandboxUID      = ^uint32(0)
	sandboxGID      = ^uint32(0)
	sandboxHomeDir  = ""
)

func initSandbox() (bool, uint32, uint32, string, error) {
	sandboxOnce.Do(func() {
		if syscall.Getuid() != 0 || os.Getenv("CI") != "" || os.Getenv("SYZ_DISABLE_SANDBOXING") == "yes" {
			sandboxEnabled = false
			return
		}
		sandboxUser, err := user.Lookup(sandboxUsername)
		if err != nil {
			return
		}
		uid, err := strconv.Atoi(sandboxUser.Uid)
		if err != nil {
			return
		}
		gid, err := strconv.Atoi(sandboxUser.Gid)
		if err != nil {
			return
		}
		sandboxUID = uint32(uid)
		sandboxGID = uint32(gid)
		sandboxHomeDir = sandboxUser.HomeDir
	})
	if sandboxEnabled && sandboxUID == ^uint32(0) {
		return false, 0, 0, "", fmt.Errorf("user %q is not found, can't sandbox command", sandboxUsername)
	}
	return sandboxEnabled, sandboxUID, sandboxGID, sandboxHomeDir, nil
}

func setPdeathsig(cmd *exec.Cmd, hardKill bool) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = new(syscall.SysProcAttr)
	}
	if hardKill {
		cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	} else {
		cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
	}
	// We will kill the whole process group.
	cmd.SysProcAttr.Setpgid = true
}

func killPgroup(cmd *exec.Cmd) {
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func prolongPipe(r, w *os.File) {
	for sz := 128 << 10; sz <= 2<<20; sz *= 2 {
		syscall.Syscall(syscall.SYS_FCNTL, w.Fd(), syscall.F_SETPIPE_SZ, uintptr(sz))
	}
}
