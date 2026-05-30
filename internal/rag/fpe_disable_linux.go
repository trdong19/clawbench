package rag

/*
#include <fenv.h>
#include <stdint.h>

// disableAllFPE clears all floating-point exception bits and masks
// all FE traps. This prevents SIGFPE crashes from DuckDB on older CPUs
// (e.g. Intel Haswell i3-4010U) where internal FP operations can trigger
// unexpected exceptions. (ISS-155)
static void disableAllFPE(void) {
    feclearexcept(FE_ALL_EXCEPT);
    fedisableexcept(FE_ALL_EXCEPT);
}
*/
import "C"

import "sync"

var fpeOnce sync.Once

// disableFPE disables all floating-point exceptions to prevent SIGFPE
// crashes on older CPUs (Haswell, etc.) where DuckDB's internal FP
// operations can trigger unexpected exceptions. Safe to call multiple
// times; only executes once per process.
func disableFPE() {
	fpeOnce.Do(func() {
		C.disableAllFPE()
	})
}
