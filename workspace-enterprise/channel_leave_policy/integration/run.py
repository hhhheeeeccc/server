#!/usr/bin/env python3
"""Run the HTTP tests with disposable PostgreSQL, never a deployment database."""

import os
import pathlib
import shutil
import subprocess
import tempfile


def main():
    enterprise = pathlib.Path(__file__).resolve().parents[2]
    server = enterprise.parent / "mattermost" / "server"
    postgres = pathlib.Path(os.environ.get("PG_BINDIR", "/usr/lib/postgresql/16/bin"))
    for executable in ("initdb", "pg_ctl", "psql"):
        if not (postgres / executable).is_file():
            raise SystemExit(f"Missing {postgres / executable}; set PG_BINDIR.")

    root = pathlib.Path(tempfile.mkdtemp(prefix="workspace-channel-leave-", dir="/tmp"))
    started = False
    try:
        subprocess.run(
            ["go", "test", "-tags", "enterprise sourceavailable integration", "-c",
             "-o", str(root / "policy.test"), str(enterprise / "channel_leave_policy" / "integration")],
            cwd=server, check=True,
        )
        with (root / "setup.log").open("w") as log:
            subprocess.run(
                [str(postgres / "initdb"), "-D", str(root / "data"), "-A", "trust",
                 "--no-locale", "--encoding=UTF8"], stdout=log, stderr=subprocess.STDOUT, check=True,
            )
            subprocess.run(
                [str(postgres / "pg_ctl"), "-D", str(root / "data"), "-l", str(root / "postgres.log"),
                 "-o", f"-k {root} -p 55493 -h '' -c fsync=off", "-w", "start"],
                stdout=log, stderr=subprocess.STDOUT, check=True,
            )
            started = True
            subprocess.run(
                [str(postgres / "psql"), "-h", str(root), "-p", "55493", "-d", "postgres",
                 "-v", "ON_ERROR_STOP=1", "-c", "CREATE ROLE mmuser SUPERUSER LOGIN"],
                stdout=log, stderr=subprocess.STDOUT, check=True,
            )

        env = {key: value for key, value in os.environ.items() if not key.startswith("MM_")}
        env["TEST_DATABASE_POSTGRESQL_DSN"] = (
            f"postgres://mmuser@:55493/postgres?host={root}&sslmode=disable"
        )
        env["MM_LOGSETTINGS_CONSOLELEVEL"] = "ERROR"
        logfile = pathlib.Path("/tmp/workspace-channel-leave-integration.log")
        print(f"Isolated PostgreSQL HTTP test; log: {logfile}", flush=True)
        with logfile.open("w") as log:
            result = subprocess.run(
                [str(root / "policy.test"), "-test.v", "-test.timeout=180s"],
                cwd=server, env=env, stdout=log, stderr=subprocess.STDOUT,
            )
        print("\n".join(logfile.read_text(errors="replace").splitlines()[-70:]), flush=True)
        return result.returncode
    finally:
        if started:
            subprocess.run(
                [str(postgres / "pg_ctl"), "-D", str(root / "data"), "-m", "fast", "-w", "stop"],
                stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT, check=True,
            )
        shutil.rmtree(root)


if __name__ == "__main__":
    raise SystemExit(main())
