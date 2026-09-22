#!/usr/bin/env python3
"""Embed video-session.py into the AppBundle manifest.

The workload runs as `python3 -u -c <program>` so that the demo stays a single
self-contained AppBundle component -- introducing a ConfigMap just to hold the
source would change the application graph the RecoveryGroup is defined over.
Keeping the program in its own .py file and injecting it here means it is still
editable, lintable and diffable as Python.

    ./gen-appbundle.py            # rewrite 20-appbundle-video-stream-service.yaml
"""
import os
import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
BUNDLE = os.path.join(HERE, "20-appbundle-video-stream-service.yaml")
PROGRAM = os.path.join(HERE, "video-session.py")


class Literal(str):
    pass


def _literal(dumper, data):
    return dumper.represent_scalar("tag:yaml.org,2002:str", data, style="|")


yaml.add_representer(Literal, _literal)

doc = yaml.safe_load(open(BUNDLE))
src = open(PROGRAM).read()

found = False
for group in doc["spec"]["groups"]:
    for comp in group["components"]:
        if comp["name"] != "video":
            continue
        for c in comp["template"]["spec"]["template"]["spec"]["containers"]:
            if c["name"] == "video":
                c["args"] = [Literal(src)]
                found = True
if not found:
    raise SystemExit("video container not found in %s" % BUNDLE)

with open(BUNDLE, "w") as f:
    yaml.dump(doc, f, sort_keys=False, default_flow_style=False, width=100)
print("embedded %d bytes of %s into %s" % (len(src), os.path.basename(PROGRAM), os.path.basename(BUNDLE)))
