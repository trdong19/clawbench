//go:build !linux

package rag

// disableFPE is a no-op on non-Linux platforms.
// The SIGFPE issue with DuckDB has only been observed on Linux (glibc).
func disableFPE() {}
