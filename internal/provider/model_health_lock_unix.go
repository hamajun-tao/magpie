//go:build !windows

package provider

import (
	"os"
	"syscall"
)

func tryModelHealthLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK {
		return false, nil
	}
	return err == nil, err
}

func unlockModelHealthFile(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
