//go:build darwin && arm64

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// buildkitRecordName is the per-instance file (inside InstanceDir) that says
// what a buildkit instance serves: the cache directory it holds the lock on
// and the host socket buildkitd answers on. It is what lets a second
// `ossein buildkit --detach` for the same project — and the docker-shaped
// front, which issues exactly that — find and reuse the running instance
// instead of tripping over its lock.
const buildkitRecordName = "buildkit.json"

// buildkitRecord is the content of buildkitRecordName.
type buildkitRecord struct {
	Cache string `json:"cache"` // the resolved cache dir (buildkitCmd.resolveCacheDir)
	Sock  string `json:"sock"`  // absolute host socket path (resolveSock)
}

// writeBuildkitRecord records what the instance in dir serves.
func writeBuildkitRecord(dir string, rec buildkitRecord) error {
	encoded, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding buildkit record: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, buildkitRecordName), encoded, pidFileMode); err != nil {
		return fmt.Errorf("writing buildkit record: %w", err)
	}

	return nil
}

// runningInstance is a live buildkit instance found by findBuildkitInstance.
type runningInstance struct {
	id    string
	dir   string
	rec   buildkitRecord
	pid   int
	start int64
}

// findBuildkitInstance scans the state root for a LIVE instance (its recorded
// pid is that same process, still running) serving cacheDir. Dead instances,
// instances without a record (a previous release), and unreadable records are
// skipped: the caller then falls through to the lock-based check, which
// reports "busy" for anything this could not identify.
func findBuildkitInstance(root, cacheDir string) (runningInstance, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return runningInstance{}, false
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dir := filepath.Join(root, entry.Name())

		raw, err := os.ReadFile(filepath.Join(dir, buildkitRecordName)) // #nosec G304 -- ossein-owned state dir
		if err != nil {
			continue
		}

		var rec buildkitRecord
		if err := json.Unmarshal(raw, &rec); err != nil || rec.Cache != cacheDir || rec.Sock == "" {
			continue
		}

		pid, start, ok := readPid(filepath.Join(dir, pidFileName))
		if !ok || !processAlive(pid, start) {
			continue
		}

		return runningInstance{id: entry.Name(), dir: dir, rec: rec, pid: pid, start: start}, true
	}

	return runningInstance{}, false
}

// reuseInstance implements the idempotent half of `ossein buildkit --detach`:
// when an instance already serves cacheDir, its BUILDKIT_HOST is printed
// again and the command succeeds without booting anything. An instance that
// is still starting (pid alive, socket not yet answering) is waited for, like
// detach waits for its own child — but never killed on timeout: it is not
// ours to reap. done is false when no live instance serves cacheDir.
func reuseInstance(ctx context.Context, logger *slog.Logger, cacheDir string) (done bool, err error) {
	root, err := stateRoot()
	if err != nil {
		return false, err
	}

	inst, found := findBuildkitInstance(root, cacheDir)
	if !found {
		return false, nil
	}

	logPath := filepath.Join(inst.dir, buildkitLogName)
	deadline := time.Now().Add(bkReadyTimeout)

	for !probeSocketChain(ctx, inst.rec.Sock) {
		if ctx.Err() != nil {
			return false, fmt.Errorf("%w: interrupted while waiting for instance %s", errBuildkit, inst.id)
		}

		if !processAlive(inst.pid, inst.start) {
			// It died while we waited: nothing serves the cache any more, so the
			// caller's own launch is the right next step.
			return false, nil
		}

		if time.Now().After(deadline) {
			return false, fmt.Errorf(
				"%w: instance %s holds this project's cache but is not answering on %s after %s — "+
					"see %s, or `ossein stop %s`",
				errBuildkit, inst.id, inst.rec.Sock, bkReadyTimeout, logPath, inst.id,
			)
		}

		time.Sleep(stopPollInterval)
	}

	fmt.Fprintf(os.Stdout, "export BUILDKIT_HOST=unix://%s\n", inst.rec.Sock)
	logger.Info("buildkit already running for this cache — reusing",
		logKeyID, inst.id, "sock", inst.rec.Sock, "stop", "ossein stop "+inst.id)

	return true, nil
}
