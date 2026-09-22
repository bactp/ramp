"""Build a CRI checkpoint OCI image from a kubelet checkpoint tar, and import it
into the node's containerd image store.

This is the image format the Transition Operator already produces with buildah
(`buildah from scratch` / `add <tar> /` / two org.criu.checkpoint.* annotations).
It is reproduced here without buildah, which is not installed on sre-control,
and imported straight into containerd so nothing has to be published to an
external registry for the experiment.

containerd's CRI reads org.criu.checkpoint.rootfsImageName from the image
MANIFEST annotations: it then creates the container with that image as the
rootfs and restores the process from the CRIU data in this image's layer.
"""
import gzip, hashlib, io, json, os, shutil, subprocess, sys, tarfile, tempfile

ckpt_tar = sys.argv[1]           # /var/lib/kubelet/checkpoints/checkpoint-....tar
image_ref = sys.argv[2]          # e.g. docker.io/ramp/checkpoint-video:epoch-7
rootfs_image = sys.argv[3]       # e.g. docker.io/library/python:3.12-slim

work = tempfile.mkdtemp(prefix="ramp-ckptimg-")
blobs = os.path.join(work, "blobs", "sha256")
os.makedirs(blobs)


def put(data: bytes) -> tuple[str, int]:
    digest = hashlib.sha256(data).hexdigest()
    with open(os.path.join(blobs, digest), "wb") as f:
        f.write(data)
    return "sha256:" + digest, len(data)


def normalise_rootfs_ref(tar_path: str) -> str:
    """Repack the checkpoint tar with a resolvable config.dump/rootfsImageRef.

    The kubelet records rootfsImageRef as whatever the node's image store
    resolved for the container. Depending on how the rootfs image got onto the
    node that can be a proper reference (docker.io/library/python@sha256:...)
    or a BARE DIGEST (sha256:...). containerd's restore path feeds that value
    straight into an image resolver, and a bare digest blows up with:

        failed to pull checkpoint base image sha256:9e8797...:
        parse "dummy://sha256:9e8797...": invalid port

    Both forms were observed on this testbed hours apart from the same
    Deployment, so this is not a corner case. Rewriting the field to
    <rootfsImageName>@<digest> keeps the exact same image identity while giving
    the resolver something it can parse.
    """
    with tarfile.open(tar_path) as t:
        cfg = json.loads(t.extractfile("config.dump").read())
    ref = cfg.get("rootfsImageRef", "")
    before = (cfg.get("rootfsImage"), cfg.get("rootfsImageName"), ref)

    # containerd's restore path PULLS rootfsImageRef from a registry -- it does
    # not look it up in the local store. Verified on this testbed:
    #   sha256:2c941e86...  Hub index digest   -> pullable, restore proceeds
    #   sha256:9e87977b...  node-local image ID -> NOT pullable, restore fails
    # and the kubelet records whichever of the two the node happened to hold.
    # A tag is always registry-resolvable, so the tag form is used unless the
    # recorded ref is already a proper repository@digest reference.
    if "/" in ref and "@" in ref:
        cfg["rootfsImage"] = cfg["rootfsImageName"] = rootfs_image
    else:
        cfg["rootfsImage"] = cfg["rootfsImageName"] = cfg["rootfsImageRef"] = rootfs_image

    if before == (cfg.get("rootfsImage"), cfg.get("rootfsImageName"), cfg.get("rootfsImageRef")):
        return tar_path
    print("  rewrote rootfs fields -> name=%s ref=%s" % (cfg["rootfsImageName"], cfg["rootfsImageRef"]))

    patched = os.path.join(work, "checkpoint.tar")
    with tarfile.open(tar_path) as src, tarfile.open(patched, "w") as dst:
        for m in src.getmembers():
            if m.name == "config.dump":
                data = json.dumps(cfg).encode()
                m.size = len(data)
                dst.addfile(m, io.BytesIO(data))
            else:
                dst.addfile(m, src.extractfile(m) if m.isfile() else None)
    return patched


ckpt_tar = normalise_rootfs_ref(ckpt_tar)

# The layer IS the checkpoint tar: `buildah add <tar> /` extracts it at root, so
# a layer whose contents are checkpoint/, config.dump, spec.dump ... is the same
# filesystem. diff_id is the digest of the UNCOMPRESSED tar.
raw = open(ckpt_tar, "rb").read()
diff_id = "sha256:" + hashlib.sha256(raw).hexdigest()
gz = gzip.compress(raw, compresslevel=1)
layer_digest, layer_size = put(gz)

config = {
    "architecture": "amd64",
    "os": "linux",
    "config": {},
    "rootfs": {"type": "layers", "diff_ids": [diff_id]},
}
config_digest, config_size = put(json.dumps(config).encode())

manifest = {
    "schemaVersion": 2,
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "config": {
        "mediaType": "application/vnd.oci.image.config.v1+json",
        "digest": config_digest,
        "size": config_size,
    },
    "layers": [{
        "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
        "digest": layer_digest,
        "size": layer_size,
    }],
    # These two annotations are the whole contract with the runtime.
    "annotations": {
        "org.criu.checkpoint.container.name": image_ref,
        "org.criu.checkpoint.rootfsImageName": rootfs_image,
    },
}
manifest_digest, manifest_size = put(json.dumps(manifest).encode())

json.dump({"imageLayoutVersion": "1.0.0"}, open(os.path.join(work, "oci-layout"), "w"))
json.dump({
    "schemaVersion": 2,
    "mediaType": "application/vnd.oci.image.index.v1+json",
    "manifests": [{
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "digest": manifest_digest,
        "size": manifest_size,
        "platform": {"architecture": "amd64", "os": "linux"},
        "annotations": {"org.opencontainers.image.ref.name": image_ref},
    }],
}, open(os.path.join(work, "index.json"), "w"))

archive = os.path.join(work, "image.tar")
with tarfile.open(archive, "w") as t:
    for name in ("oci-layout", "index.json", "blobs"):
        t.add(os.path.join(work, name), arcname=name)

print("built OCI checkpoint image archive: %s (layer %.1f MB)" % (archive, layer_size / 1e6))
print("  rootfsImageName =", rootfs_image)
print("  container.name  =", image_ref)

# Import into the k8s.io namespace so the kubelet/CRI can use it by tag.
# No --no-unpack: containerd must unpack the layer into a snapshot, otherwise
# the kubelet fails to create the container with
#   "parent snapshot sha256:... does not exist: not found"
out = subprocess.run(
    ["ctr", "-n", "k8s.io", "images", "import", archive],
    capture_output=True, text=True)
print(out.stdout.strip() or out.stderr.strip())
if out.returncode != 0:
    shutil.rmtree(work, ignore_errors=True)
    sys.exit(out.returncode)

shutil.rmtree(work, ignore_errors=True)
print("imported", image_ref)
