// Package vmexec is the ossein guest container runtime — the Go port of Apple's
// vmexec. It runs as a re-exec'd `stage2` child of the agent: the agent launches
// /proc/self/exe with the container's namespaces already applied via clone, and
// stage2 performs the in-namespace setup (stdio, rootfs pivot, mounts, caps,
// uid/gid) then execs the workload.
//
// The re-exec replaces vmexec's unshare()+fork(): Go cannot run arbitrary code
// after fork(), so namespaces are created at clone time (SysProcAttr.Cloneflags
// on the agent side) and this fresh process does the rest, mirroring the order
// of vmexec's RunCommand.childSetup.
//
// The package doc lives in this portable file rather than stage2.go (linux-only)
// so the package is documented on every GOOS the native analysis runs under.
package vmexec
