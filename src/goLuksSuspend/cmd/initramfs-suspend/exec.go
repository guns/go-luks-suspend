package main

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	g "goLuksSuspend"

	"github.com/guns/golibs/editreader"
	"github.com/guns/golibs/sys"
)

func loadCryptdevices(r io.Reader) (cryptdevs []g.Cryptdevice, err error) {
	err = gob.NewDecoder(r).Decode(&cryptdevs)
	return cryptdevs, err
}

func suspendCryptdevices(cryptdevs []g.Cryptdevice) error {
	// Iterate backwards so that we suspend the root device last. This prevents
	// a logical deadlock in which a cryptdevice is actually a file on the root
	// device. There is no way of solving this problem in the general case
	// without building a directed graph of cryptdevices -> cryptdevices.
	for i := len(cryptdevs) - 1; i >= 0; i-- {
		g.Debug("suspending " + cryptdevs[i].Name)
		if err := g.Cryptsetup("luksSuspend", cryptdevs[i].Name); err != nil {
			return err
		}
	}

	return nil
}

func startUdevDaemon() error {
	return exec.Command("/usr/lib/systemd/systemd-udevd", "--daemon", "--resolve-names=never").Run()
}

func stopUdevDaemon() error {
	return exec.Command("/usr/bin/udevadm", "control", "--exit").Run()
}

func printPassphrasePrompt(rootdev *g.Cryptdevice) {
	fmt.Print("\nPress Escape to suspend to RAM")
	if rootdev.Keyfile.Defined() {
		fmt.Print(", or Ctrl-R to rescan block devices for keyfiles")
	}
	if g.DebugMode {
		fmt.Print(", or Ctrl-T to start a debug shell")
	}
	fmt.Println(".")
	fmt.Printf("\nEnter passphrase for %s: ", rootdev.Name)
}

func luksResume(dev *g.Cryptdevice, stdin io.Reader) error {
	if dev.Keyfile.Available() {
		if err := dev.ResumeWithKeyfile(); err == nil {
			return nil
		}
	}

	printPassphrasePrompt(dev)
	return dev.Resume(stdin)
}

func resumeRootCryptdevice(rootdev *g.Cryptdevice) error {
	restoreTTY, err := sys.AlterTTY(os.Stdin.Fd(), sys.TCSETSF, func(tty syscall.Termios) syscall.Termios {
		tty.Lflag &^= syscall.ICANON | syscall.ECHO
		return tty
	})

	ttyRestored := false

	if restoreTTY != nil {
		defer func() {
			if !ttyRestored {
				g.Assert(restoreTTY())
			}
		}()
	}

	if err != nil {
		g.Warn(err.Error())
		return luksResume(rootdev, os.Stdin)
	}

	// The `secure` parameter to editreader.New zeroes memory aggressively
	r := editreader.New(os.Stdin, 4096, true, func(i int, b byte) editreader.Op {
		switch b {
		case 0x1b: // ^[
			g.Debug("suspending to RAM")
			g.Assert(g.SuspendToRAM())
			fmt.Println()
			printPassphrasePrompt(rootdev)
			return editreader.Kill
		case 0x17: // ^W
			return editreader.Kill
		case '\n':
			fmt.Println()
			g.Assert(restoreTTY())
			ttyRestored = true
			return editreader.Append | editreader.Flush | editreader.Close
		case 0x12: // ^R
			if rootdev.Keyfile.Defined() {
				if rootdev.Keyfile.Available() {
					fmt.Printf("\nAttempting to unlock %s with keyfile...\n", rootdev.Name)
				} else {
					fmt.Println("\nKeyfile unavailable.")
				}
				return editreader.Kill | editreader.Flush | editreader.Close
			}
			return editreader.BasicLineEdit(i, b)
		case 0x14: // ^T
			if g.DebugMode {
				fmt.Println()
				g.DebugShell()
				printPassphrasePrompt(rootdev)
				return editreader.Kill
			}
			fallthrough
		default:
			return editreader.BasicLineEdit(i, b)
		}
	})

	return luksResume(rootdev, r)
}
