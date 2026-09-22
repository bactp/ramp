"""Turn the checkpoint's spec.dump into a restorable runc bundle config.json.

The dumped spec describes the container as it existed inside a pod sandbox on
the SOURCE node. Two classes of things in it cannot survive the move to another
cluster, and each needs a different treatment:

  * bind mounts  -- their `source` paths (serviceaccount projected volume,
    /etc/hosts, /etc/resolv.conf, the termination log) live under
    /var/lib/kubelet/pods/<uid>/... on workload01 and do not exist here.
    They are RE-POINTED at freshly created local stand-ins rather than dropped:
    CRIU rebuilds the container's mount namespace from mountpoints-*.img and
    needs a mount at each destination, so deleting them makes the restore fail
    with an opaque "criu failed: type RESTORE errno 0".

  * namespace paths -- pinned to /proc/<pid>/ns/... of the source sandbox.
    Dropping the path makes runc create a fresh namespace of that type.
"""
import json, os, sys

w = sys.argv[1]
spec = json.load(open(os.path.join(w, "ckpt", "spec.dump")))
spec["root"] = {"path": "rootfs", "readonly": False}

stand_ins = os.path.join(w, "mounts")
os.makedirs(stand_ins, exist_ok=True)

# Destinations that are files on the source; everything else is a directory.
FILE_DESTS = {"/etc/hosts", "/etc/hostname", "/etc/resolv.conf",
              "/dev/termination-log"}

kernel_types = {"proc", "sysfs", "tmpfs", "devpts", "mqueue", "cgroup"}
mounts, rehomed = [], 0
for i, m in enumerate(spec.get("mounts", [])):
    if m.get("type") in kernel_types:
        mounts.append(m)
        continue
    dest = m.get("destination", "")
    src = os.path.join(stand_ins, "m%02d" % i)
    if dest in FILE_DESTS:
        os.makedirs(os.path.dirname(src), exist_ok=True)
        if not os.path.exists(src):
            open(src, "w").close()
    else:
        os.makedirs(src, exist_ok=True)
    m = dict(m)
    m["source"] = src
    mounts.append(m)
    rehomed += 1
spec["mounts"] = mounts

linux = spec.setdefault("linux", {})
linux["namespaces"] = [{"type": n["type"]} for n in linux.get("namespaces", [])]
# The network namespace is deliberately left isolated rather than joined to the
# host's: restoring the container's interfaces and addresses into the node netns
# could disturb the target node. Consequence documented in 04-results.md.
linux.pop("resources", None)
linux.pop("cgroupsPath", None)
spec.pop("cgroupsPath", None)
spec["hostname"] = "ramp-restored"
spec["process"]["terminal"] = False

json.dump(spec, open(os.path.join(w, "bundle", "config.json"), "w"), indent=1)
print("bundle spec written: %d mounts (%d re-homed), %d namespaces"
      % (len(mounts), rehomed, len(linux["namespaces"])))
