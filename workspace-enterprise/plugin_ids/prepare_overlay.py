#!/usr/bin/env python3
"""Prepare Workspace plugin IDs for go build without editing server sources."""

import argparse
import json
from pathlib import Path
import re


def prepare(server: Path, output: Path) -> Path:
    source = (server / "public/model/plugin_constants.go").resolve(strict=True)
    text = source.read_text()
    identifiers = {
        "PluginIdPlaybooks": "workspace",
        "PluginIdApps": "com.workspace.apps",
        "PluginIdCalls": "com.workspace.calls",
        "PluginIdNPS": "com.workspace.nps",
        "PluginIdAI": "workspace-ai",
    }
    for name, value in identifiers.items():
        text, count = re.subn(
            rf'(?m)^(\s*{name}\s*=\s*)"[^"\n]*"',
            lambda match: match[1] + json.dumps(value),
            text,
        )
        if count != 1:
            raise ValueError(f"Expected exactly one declaration of {name}, found {count}")
    output = output.resolve()
    if output.is_relative_to(server.resolve()):
        raise ValueError("Overlay output must be outside the server directory")
    output.mkdir(parents=True, exist_ok=True)
    replacement = output / "plugin_constants.go.txt"
    replacement.write_text(text)
    overlay = output / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(source): str(replacement)}}, indent=2) + "\n")
    return overlay


if __name__ == "__main__":
    feature = Path(__file__).resolve().parent
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=feature.parent.parent / "mattermost/server")
    parser.add_argument("--output", type=Path, default=feature / "build_overlay")
    args = parser.parse_args()
    print(prepare(args.server, args.output))
