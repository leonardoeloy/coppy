//go:build !windows

package main

import "syscall"

func detach() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
