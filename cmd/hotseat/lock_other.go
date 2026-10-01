//go:build !unix

package main

import "os"

func tryLock(*os.File) bool { return true }
