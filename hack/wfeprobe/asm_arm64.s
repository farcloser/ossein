#include "textflag.h"

// func sevlWfe(n int64)
// SEVL sets the local event register, so the following WFE returns immediately.
// If the hypervisor traps WFE (HCR_EL2.TWE), each iteration costs a VM exit.
TEXT ·sevlWfe(SB), NOSPLIT, $0-8
	MOVD n+0(FP), R0
loop:
	WORD $0xd50320bf	// SEVL (HINT #5)
	WORD $0xd503205f	// WFE
	SUBS $1, R0
	BNE  loop
	RET

// func spinWait(addr *uint32, want uint32)
// Plain acquire-load spin — the baseline: cross-CPU store -> observe.
TEXT ·spinWait(SB), NOSPLIT, $0-12
	MOVD addr+0(FP), R0
	MOVW want+8(FP), R1
spin:
	LDARW (R0), R2
	CMPW  R1, R2
	BEQ   done
	JMP   spin
done:
	RET

// func wfeWait(addr *uint32, want uint32)
// Park in WFE with the exclusive monitor armed on addr: LDAXR arms it, a remote
// store to addr clears it and generates a local event, waking WFE. This is the
// primitive polling idle would use (__cmpwait / smp_cond_load_relaxed).
TEXT ·wfeWait(SB), NOSPLIT, $0-12
	MOVD addr+0(FP), R0
	MOVW want+8(FP), R1
wloop:
	LDAXRW (R0), R2		// arm the exclusive monitor
	CMPW   R1, R2
	BEQ    wdone
	WORD   $0xd503205f	// WFE — sleeps until the monitor is cleared by a store
	JMP    wloop
wdone:
	CLREX  $0
	RET
