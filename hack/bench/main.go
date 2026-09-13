//go:build linux

// Command bench is the ossein guest microbenchmark suite: one static linux/arm64
// binary that runs a single benchmark selected by --type, so the whole suite is a
// single artifact to cross-build and mount into a container. Each benchmark lives
// in its own file (syscall.go, forkexec.go, forkonly.go, fs.go, fileio.go) as a
// run<Name> function; this file only dispatches. Positional args after --type are
// the selected benchmark's own args, e.g. `bench --type=fs rootfs stat 20000`.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	// Fast path for the forkexec child: it re-execs this binary as `<self> noop`
	// (see forkexec.go) and must exit immediately, before any flag parsing.
	if len(os.Args) > 1 && os.Args[1] == "noop" {
		os.Exit(0)
	}

	// Only benchmarks with NO `perf bench` equivalent live here — perf covers raw syscall
	// (syscall basic), fork (syscall fork), and execve (syscall execve), so those were
	// removed in favor of bench-perf. What remains is what perf cannot do: exec-from-mount
	// (forkexec's virtio path), virtio-fs metadata/data (fs), small-file writes (fileio),
	// and page-table teardown/fault (mmap).
	typ := flag.String("type", "", "benchmark to run: forkexec | fs | fileio | mmap")

	flag.Parse()

	args := flag.Args()

	// Every benchmark returns its failure rather than exiting: main is the only
	// place that ends the process, so a run's deferred cleanup (temp dirs, staged
	// exec targets, mapped regions) actually happens on the error path.
	var err error

	switch *typ {
	case "forkexec":
		err = runForkexec(args)
	case "fs":
		err = runFS(args)
	case "fileio":
		err = runFileio(args)
	case "mmap":
		err = runMmap(args)
	default:
		// Usage error keeps its own status, distinct from a benchmark that ran
		// and failed.
		fmt.Fprintf(os.Stderr, "bench: unknown --type %q (want: forkexec | fs | fileio | mmap)\n", *typ)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}
