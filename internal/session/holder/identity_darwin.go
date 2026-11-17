package holder

import "fmt"

// IsHolder identifies the holder protocol in a live process's argv.
func IsHolder(pid int) (bool, error) {
	args := procArgs(pid)
	if len(args) == 0 {
		return false, fmt.Errorf("cannot inspect process %d arguments", pid)
	}
	_, ok := fromArgv(pid, args)
	return ok, nil
}
