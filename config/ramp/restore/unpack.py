"""Unpack an exported OCI image into the runc bundle rootfs."""
import json, os, sys, tarfile

w = sys.argv[1]
oci = os.path.join(w, "oci")


def blob(digest):
    return os.path.join(oci, "blobs", *digest.split(":"))


index = json.load(open(os.path.join(oci, "index.json")))
manifest = json.load(open(blob(index["manifests"][0]["digest"])))
if "manifests" in manifest:  # an image index: pick the amd64 manifest
    pick = next(m for m in manifest["manifests"]
                if m.get("platform", {}).get("architecture", "amd64") == "amd64")
    manifest = json.load(open(blob(pick["digest"])))

root = os.path.join(w, "bundle", "rootfs")
for layer in manifest["layers"]:
    with tarfile.open(blob(layer["digest"])) as t:
        t.extractall(root, filter="tar")
print("rootfs unpacked from %d layers" % len(manifest["layers"]))
