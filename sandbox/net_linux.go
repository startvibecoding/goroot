//go:build linux

package sandbox

import "golang.org/x/sys/unix"

// setLoopbackUp brings the loopback interface up inside a private network
// namespace. Without this, localhost is unreachable.
func setLoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	flags := ifr.Uint16()
	flags |= unix.IFF_UP | unix.IFF_RUNNING
	ifr.SetUint16(flags)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	return err == nil
}
