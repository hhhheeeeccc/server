#!/usr/bin/env python3
"""Exercise the real Workspace cache code against disposable local Redis."""
import json
import os
import pathlib
import secrets
import subprocess


def main():
    endpoint = subprocess.check_output(
        ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"], text=True
    ).strip()
    if endpoint != "unix:///var/run/docker.sock" or os.environ.get("DOCKER_HOST"):
        raise SystemExit("Use the local Docker Unix socket for this test.")
    root = pathlib.Path(__file__).resolve().parent
    server = root.parent.parent / "mattermost" / "server"
    name = "workspace-cache-test-" + secrets.token_hex(5)
    password = secrets.token_urlsafe(24)
    subprocess.run(
        ["docker", "run", "--rm", "-d", "--name", name, "-p", "127.0.0.1::6379",
         "redis:7.4.0", "redis-server", "--save", "", "--appendonly", "no", "--requirepass", password],
        check=True, stdout=subprocess.DEVNULL,
    )
    try:
        bindings = json.loads(subprocess.check_output(
            ["docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", name], text=True
        ))
        port = bindings["6379/tcp"][0]["HostPort"]
        subprocess.run(["docker", "exec", "-e", "REDISCLI_AUTH=" + password, name, "redis-cli", "ping"], check=True)
        env = os.environ.copy()
        env["WORKSPACE_TEST_REDIS_ADDRESS"] = "127.0.0.1:" + port
        env["WORKSPACE_TEST_REDIS_PASSWORD"] = password
        logfile = pathlib.Path("/tmp/workspace-cache-validation.log")
        print("Testing isolated Redis 7.4.0; log:", logfile, flush=True)
        with logfile.open("w") as log:
            result = subprocess.run(
                ["go", "test", "-race", str(root), "-count=1", "-v", "-timeout=90s"],
                cwd=server, env=env, stdout=log, stderr=subprocess.STDOUT,
            )
        print(logfile.read_text(), flush=True)
        return result.returncode
    finally:
        subprocess.run(["docker", "rm", "-f", name], check=True, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    raise SystemExit(main())
