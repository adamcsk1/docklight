"""Select production images affected by a GitHub push or pull request."""

import json
import os
import subprocess


def select_targets(paths):
    selected = set()
    for path in paths:
        if path.startswith("cmd/docklight/"):
            selected.add("ui")
        elif path.startswith("cmd/docklight-agent/"):
            selected.add("agent")
        elif (
            path.startswith(("docs/", ".opencode/"))
            or path.endswith(".md")
            or path in ("LICENSE", ".gitignore", "docker-compose.development.yml", "scripts/ui-smoke.cjs")
        ):
            continue
        else:
            # Unknown build inputs must not silently leave a published image stale.
            selected.update(("ui", "agent"))
    return [target for target in ("ui", "agent") if target in selected]


def changed_targets(event, ref, head):
    if ref.startswith("refs/tags/"):
        return ["ui", "agent"]
    pr = event.get("pull_request")
    base = pr["base"]["sha"] if pr else event.get("before")
    if not base or not base.strip("0"):
        return ["ui", "agent"]
    revision = f"{base}...{pr['head']['sha']}" if pr else f"{base}..{head}"
    try:
        files = subprocess.check_output(
            ["git", "diff", "--name-only", "--no-renames", "-z", revision, "--"],
            stderr=subprocess.DEVNULL,
        )
    except subprocess.CalledProcessError:
        # New branches or force pushes can reference history missing from checkout.
        return ["ui", "agent"]
    return select_targets(files.decode("utf-8", errors="surrogateescape").split("\0")[:-1])


if __name__ == "__main__":
    with open(os.environ["GITHUB_EVENT_PATH"], encoding="utf-8") as event_file:
        event = json.load(event_file)
    print(json.dumps(changed_targets(event, os.environ["GITHUB_REF"], os.environ["GITHUB_SHA"])))
