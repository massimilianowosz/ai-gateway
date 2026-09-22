//go:build windows

package cli

import "os/exec"

func setSysProcAttr(cmd *exec.Cmd) {
	// No Setpgid on Windows; process groups handled differently.
}
