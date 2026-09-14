// ossein instance lifecycle: a model of the INTENT, not of the code.
//
// Sources, in order of authority: `ossein <cmd> --help`, README.md ("Persistent
// build cache", "Operational notes"), docs/TODO.md (the M3/M4 acceptance
// criteria: "kill -9 the daemon -> ossein gc leaves no orphans"). Written
// source-blind: nothing under cmd/, internal/, pkg/ or guest/ was read. Where
// the public surface is silent the model picks the stricter reading and marks
// it `Q<n>`; the questions are listed in spec/README.md.
//
// Check:   quint typecheck spec/lifecycle.qnt
// Explore: quint run spec/lifecycle.qnt --backend=typescript --invariants=all
// Traces:  quint run spec/lifecycle.qnt --backend=typescript --mbt \
//            --out-itf='spec/traces/{seq}.itf.json' --max-samples=200
module lifecycle {
  // --- domains: small and finite so the simulator can enumerate them ---
  type CacheKind = Central | Local
  type Cache = { kind: CacheKind, key: str }
  type Image = str
  type Policy = Always | Missing | Never
  // Live: the ossein process owning the VM is running.
  // Dead: the process is gone (kill -9, host crash) but its state entry remains.
  type Health = Live | Dead
  type Instance = { cache: Cache, health: Health }
  type Result = Ok | Err(str)

  pure val IDS: Set[str] = Set("i1", "i2", "i3")
  pure val CACHES: Set[Cache] = Set(
    { kind: Central, key: "dirA" },   // --cache omitted: keyed by the project directory
    { kind: Central, key: "api" },    // --cache api: a bare name
    { kind: Local, key: "./.ossein/cache" }
  )
  pure val IMAGES: Set[Image] = Set("debian", "alpine")

  // --- state: what an operator can observe through the public surface ---
  var instances: str -> Instance   // backgrounded buildkit instances with a state-dir entry (Q1)
  var caches: Set[Cache]           // cache volumes that exist on disk
  var images: Set[Image]           // images present in the local image cache
  var localEver: Set[Cache]        // history: every project-local cache ever created
  var last: { name: str, result: Result, instancesBefore: str -> Instance }

  // --- derived ---
  def live(id: str): bool = instances.get(id).health == Live
  val liveInstances: Set[str] = instances.keys().filter(id => live(id))
  def cacheInUse(c: Cache): bool = liveInstances.exists(id => instances.get(id).cache == c)
  pure def without(m: str -> Instance, ids: Set[str]): str -> Instance =
    m.keys().exclude(ids).mapBy(id => m.get(id))

  action record(name: str, r: Result): bool =
    last' = { name: name, result: r, instancesBefore: instances }

  action init = all {
    instances' = Map(),
    caches' = Set(),
    images' = Set(),
    localEver' = Set(),
    last' = { name: "init", result: Ok, instancesBefore: Map() },
  }

  // `ossein buildkit --detach [--cache ...]`
  // README: "a cache volume is locked while in use; a second ossein buildkit for
  // the same cache fails fast". Intent read strictly: only a LIVE instance holds
  // the lock (Q2: a dead holder must not block).
  action buildkitDetach = {
    nondet id = IDS.oneOf()
    nondet c = CACHES.oneOf()
    any {
      all {
        cacheInUse(c),
        record("buildkitDetach", Err("cache in use")),
        instances' = instances, caches' = caches, images' = images, localEver' = localEver,
      },
      all {
        not(cacheInUse(c)),
        not(instances.keys().contains(id)),
        instances' = instances.put(id, { cache: c, health: Live }),
        caches' = caches.union(Set(c)),
        localEver' = if (c.kind == Local) localEver.union(Set(c)) else localEver,
        images' = images,
        record("buildkitDetach", Ok),
      },
    }
  }

  // `ossein stop <id>`: SIGTERM, wait --grace, SIGKILL. Either way the instance
  // is gone afterwards and its state entry with it. Stopping an unknown id: Q3.
  // Stopping a dead instance reaps its entry: Q4.
  action stopOne = {
    nondet id = IDS.oneOf()
    all {
      instances.keys().contains(id),
      instances' = instances.without(Set(id)),
      caches' = caches, images' = images, localEver' = localEver,
      record("stop", Ok),
    }
  }

  // `ossein stop` with no ids: all instances.
  action stopAll = all {
    instances' = Map(),
    caches' = caches, images' = images, localEver' = localEver,
    record("stopAll", Ok),
  }

  // Environment, not a command: the owning process dies without cleanup.
  action crash = {
    nondet id = IDS.oneOf()
    all {
      instances.keys().contains(id),
      live(id),
      instances' = instances.set(id, { cache: instances.get(id).cache, health: Dead }),
      caches' = caches, images' = images, localEver' = localEver,
      record("crash", Ok),
    }
  }

  // `ossein gc`: "remove stale instance state". Never touches caches.
  action gc = all {
    instances' = instances.without(instances.keys().filter(id => not(live(id)))),
    caches' = caches, images' = images, localEver' = localEver,
    record("gc", Ok),
  }

  // `ossein gc --prune-cache`: gc, plus delete CENTRAL caches not in use.
  // README: "Project-local caches live in your tree, so they're yours to manage".
  action gcPrune = all {
    instances' = instances.without(instances.keys().filter(id => not(live(id)))),
    caches' = caches.filter(c => c.kind == Local or cacheInUse(c)),
    images' = images, localEver' = localEver,
    record("gcPrune", Ok),
  }

  // `ossein pull <image>`: populates the image cache, nothing else. The
  // registry may be unreachable (Q5: is a failed pull observable as an error
  // with the cache untouched?).
  action pull = {
    nondet img = IMAGES.oneOf()
    any {
      all {
        images' = images.union(Set(img)),
        instances' = instances, caches' = caches, localEver' = localEver,
        record("pull", Ok),
      },
      all {
        images' = images,
        instances' = instances, caches' = caches, localEver' = localEver,
        record("pull", Err("registry unreachable")),
      },
    }
  }

  // `ossein run --pull=<policy> <image> ...`: one-shot. "--rm: no-op; the
  // container is always removed on exit". Leaves instances and caches alone.
  action runOnce = {
    nondet img = IMAGES.oneOf()
    nondet policy = Set(Always, Missing, Never).oneOf()
    any {
      all {
        policy == Never, not(images.contains(img)),
        images' = images,
        instances' = instances, caches' = caches, localEver' = localEver,
        record("run", Err("image not local")),
      },
      all {
        policy == Never, images.contains(img),
        images' = images,
        instances' = instances, caches' = caches, localEver' = localEver,
        record("run", Ok),
      },
      all {
        policy == Missing,
        images' = images.union(Set(img)),   // Q6: skips the registry when local
        instances' = instances, caches' = caches, localEver' = localEver,
        record("run", Ok),
      },
      all {
        policy == Always,
        images' = images.union(Set(img)),
        instances' = instances, caches' = caches, localEver' = localEver,
        record("run", Ok),
      },
    }
  }

  // `ossein doctor`, `ossein buildkit --print-cache`, `ossein version`:
  // observe or probe, change nothing. TODO.md M1: doctor "leaves zero state
  // behind, repeatedly".
  action probe = all {
    instances' = instances, caches' = caches, images' = images, localEver' = localEver,
    record("probe", Ok),
  }

  action step = any { buildkitDetach, stopOne, stopAll, crash, gc, gcPrune, pull, runOnce, probe }

  // --- invariants: the intent, as things that must never be observed ---

  // README "One writer": two live instances never share a cache volume.
  val singleWriter = liveInstances.forall(a =>
    liveInstances.forall(b => a == b or instances.get(a).cache != instances.get(b).cache))

  // A live instance's cache volume exists on disk.
  val liveCacheExists = liveInstances.forall(id => caches.contains(instances.get(id).cache))

  // Project-local caches are never deleted by ossein.
  val localNeverDeleted = localEver.subseteq(caches)

  // TODO.md M3: after gc, no orphans.
  val gcReaps = (last.name == "gc" or last.name == "gcPrune") implies
    instances.keys().forall(id => live(id))

  // run, pull, doctor, print-cache, version never create or destroy an instance.
  val oneShotIsStateless = Set("run", "pull", "probe").contains(last.name) implies
    instances == last.instancesBefore

  // A failed command changes nothing an operator can see (Q7).
  val failureIsAtomic = (last.result != Ok) implies instances == last.instancesBefore

  // A fail-fast refusal is only ever caused by a LIVE holder (Q2).
  val refusalMeansLiveHolder = (last.name == "buildkitDetach" and last.result != Ok) implies
    liveInstances.size() > 0

  val all_invariants = all {
    singleWriter, liveCacheExists, localNeverDeleted, gcReaps,
    oneShotIsStateless, failureIsAtomic, refusalMeansLiveHolder,
  }
}
