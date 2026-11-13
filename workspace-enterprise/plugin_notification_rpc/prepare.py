#!/usr/bin/env python3
"""Build a notification RPC fix in Enterprise, preserving original repositories."""

import argparse
import json
import shutil
from pathlib import Path


def prepare(source: Path, output: Path) -> None:
    source = source.resolve(strict=True)
    output = output.resolve()
    feature = Path(__file__).resolve().parent
    enterprise = feature.parent
    if not output.is_relative_to(enterprise / "artifacts"):
        raise ValueError("Build output must be inside Enterprise artifacts")
    if output.exists():
        raise ValueError("Use a new snapshot directory; refusing to overwrite a build")
    manifest = json.loads((source / "plugin.json").read_text())
    if manifest["id"] != "workspace-tickets" or manifest["version"] != "0.39.3":
        raise ValueError("This compatibility patch targets workspace-tickets 0.39.3")
    shutil.copytree(source / "server", output / "server", ignore=shutil.ignore_patterns("dist"))
    for name in ("go.mod", "go.sum"):
        shutil.copy2(source / name, output / name)
    package = output / "server/internal/servicedesk"
    filename = package / "notification_card.go"
    text = filename.read_text()
    old = '"workspace_service_notice": card'
    if text.count(old) != 1:
        raise ValueError("Notification payload changed; review before rebuilding")
    filename.write_text(text.replace(old, '"workspace_service_notice": serviceNotificationRPCProps(card)'))
    for name in ("notification_props.go", "notification_rpc_test.go"):
        shutil.copy2(feature / (name + ".txt"), package / name)
    test = package / "notification_card_test.go"
    text = test.read_text()
    old = 'card := post.Props["workspace_service_notice"].(serviceNotificationCard)'
    if text.count(old) != 1:
        raise ValueError("Notification contract test changed; review before rebuilding")
    test.write_text(text.replace(old, '''var card serviceNotificationCard
	cardJSON, cardErr := json.Marshal(post.Props["workspace_service_notice"])
	require.NoError(t, cardErr)
	require.NoError(t, json.Unmarshal(cardJSON, &card))'''))
    print(output)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    prepare(args.source, args.output)
