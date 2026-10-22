package drivertest

import "syscall"

// dup2 makes newfd a copy of oldfd. Linux has dup3 on every architecture,
// while syscall.Dup2 is missing on some (arm64, riscv64, loong64). dup3
// refuses oldfd == newfd (EINVAL), where dup2 does nothing once oldfd is
// valid, so that case only checks oldfd.
func dup2(oldfd, newfd int) error {
	if oldfd == newfd {
		var st syscall.Stat_t
		return syscall.Fstat(oldfd, &st)
	}
	return syscall.Dup3(oldfd, newfd, 0)
}
