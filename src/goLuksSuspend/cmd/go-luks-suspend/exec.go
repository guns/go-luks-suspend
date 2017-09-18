package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"

	g "goLuksSuspend"

	"github.com/guns/golibs/errjoin"
)

func checkRootOwnedAndExecutablePath(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}

	return checkRootOwnedAndExecutable(fi)
}

func checkRootOwnedAndExecutable(fi os.FileInfo) error {
	mode := fi.Mode()

	switch {
	case !mode.IsRegular():
		return fmt.Errorf("%s is not a regular file", fi.Name())
	case mode&0022 != 0:
		return fmt.Errorf("%s is writable by group or world", fi.Name())
	case mode&0111 == 0:
		return fmt.Errorf("%s is not executable", fi.Name())
	}

	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s stat_t missing", fi.Name())
	} else if stat.Uid != 0 {
		return fmt.Errorf("%s is not owned by root", fi.Name())
	}

	return nil
}

var bindDirs = []string{"/sys", "/proc", "/dev", "/run"}

func unbindInitramfs() error {
	for _, dir := range bindDirs {
		d := filepath.Join(initramfsDir, dir)
		err := syscall.Unmount(d, 0)
		if err != nil {
			return err
		}
	}
	return nil
}

func bindInitramfs() error {
	for _, dir := range bindDirs {
		err := syscall.Mount(dir, filepath.Join(initramfsDir, dir), "", syscall.MS_BIND, "")
		if err != nil {
			return err
		}
	}
	return nil
}

const systemSleepDir = "/usr/lib/systemd/system-sleep"

// systemd-suspend.service(8):
// Immediately before entering system suspend and/or hibernation
// systemd-suspend.service (and the other mentioned units, respectively)
// will run all executables in /usr/lib/systemd/system-sleep/ and pass two
// arguments to them. The first argument will be "pre", the second either
// "suspend", "hibernate", or "hybrid-sleep" depending on the chosen action.
// Immediately after leaving system suspend and/or hibernation the same
// executables are run, but the first argument is now "post". All executables
// in this directory are executed in parallel, and execution of the action is
// not continued until all executables have finished.
func runSystemSuspendScripts(scriptarg string) error {
	dir, err := os.Open(systemSleepDir)
	if err != nil {
		return err
	}

	fs, err := dir.Readdir(0)
	if err != nil {
		return err
	}

	if err := dir.Close(); err != nil {
		return err
	}

	errslice := make([]error, len(fs))
	wg := sync.WaitGroup{}

	for i := range fs {
		if err := checkRootOwnedAndExecutable(fs[i]); err != nil {
			g.Warn(err.Error())
			continue
		}

		wg.Add(1)
		go func(i int) {
			script := filepath.Join(systemSleepDir, fs[i].Name())
			err := exec.Command(script, scriptarg, "suspend").Run()
			if err != nil {
				errslice[i] = errors.New(script + ": " + err.Error())
			}
			wg.Done()
		}(i)
	}

	wg.Wait()

	return errjoin.Join(" • ", errslice...)
}

var systemctlPath = "/usr/bin/systemctl"

func stopSystemServices(services []string) (stoppedServices []string, err error) {
	// Stopping one service may deactivate another so it is necessary to
	// record which services are active first
	for _, s := range services {
		if exec.Command(systemctlPath, "--quiet", "is-active", s).Run() == nil {
			stoppedServices = append(stoppedServices, s)
		}
	}

	args := append([]string{"stop"}, stoppedServices...)

	return stoppedServices, exec.Command(systemctlPath, args...).Run()
}

func startSystemServices(services []string) error {
	return exec.Command(systemctlPath, append([]string{"start"}, services...)...).Run()
}

func disableWriteBarriers(filesystems []filesystem) error {
	for i := range filesystems {
		if err := filesystems[i].disableWriteBarrier(); err != nil {
			return err
		}
	}
	return nil
}

func enableWriteBarriers(filesystems []filesystem) {
	for i := range filesystems {
		// The underlying device may have disappeared
		if !filesystems[i].isMounted() {
			g.Warn("[WARNING] missing filesystem mounted at " + filesystems[i].mountpoint)
			continue
		}
		if err := filesystems[i].enableWriteBarrier(); err != nil {
			g.Warn(fmt.Sprintf(
				"[WARNING] mount %s REMOUNT BARRIER: %s",
				filesystems[i].mountpoint,
				err.Error(),
			))
		}
	}
}

func suspendCmdline(debug, poweroff bool) []string {
	args := []string{"/suspend"}
	if debug {
		args = append(args, "-debug")
	}
	if poweroff {
		args = append(args, "-poweroff")
	}
	return append(args, filepath.Join("run", filepath.Base(cryptdevicesPath)))
}

func dumpCryptdevices(path string, cryptdevs []g.Cryptdevice) error {
	buf := make([][]byte, len(cryptdevs))
	j := 1

	for i := range cryptdevs {
		if cryptdevs[i].IsRootDevice {
			if len(buf[0]) > 0 {
				return fmt.Errorf(
					"multiple root cryptdevices: %s, %s",
					string(buf[0]),
					cryptdevs[i].Name,
				)
			}
			buf[0] = []byte(cryptdevs[i].Name)
		} else if j >= len(buf) {
			return errors.New("no root cryptdevice")
		} else {
			buf[j] = []byte(cryptdevs[i].Name)
			j++
		}
	}

	return ioutil.WriteFile(path, bytes.Join(buf, []byte{0}), 0600)
}

func runInInitramfsChroot(cmdline []string) error {
	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: initramfsDir}
	cmd.Dir = "/"
	cmd.Env = []string{}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func resumeDevicesWithKeyfiles(cryptdevs []g.Cryptdevice) {
	n := runtime.NumCPU()
	wg := sync.WaitGroup{}
	ch := make(chan *g.Cryptdevice)

	wg.Add(1)
	go func() {
		for i := range cryptdevs {
			ch <- &cryptdevs[i]
		}
		close(ch)
		wg.Done()
	}()

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			for cd := range ch {
				if !cd.Suspended() {
					continue
				} else if !cd.Exists() {
					g.Warn("[WARNING] missing cryptdevice " + cd.Name)
					continue
				} else if !cd.Keyfile.Exists() {
					g.Warn(fmt.Sprintf("[WARNING] no keyfile for cryptdevice %s; skipping", cd.Name))
					continue
				}

				g.Warn("Resuming " + cd.Name)

				err := cd.ResumeWithKeyfile()
				if err != nil {
					g.Warn(fmt.Sprintf("[ERROR] failed to resume %s: %s", cd.Name, err.Error()))
				} else {
					g.Warn(cd.Name + " resumed")
				}
			}
			wg.Done()
		}()
	}

	wg.Wait()
}
