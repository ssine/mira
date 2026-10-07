"""Adapt an official DeepSeek Codex catalog into a new, isolated Codex home."""
import argparse
import json
import os
from pathlib import Path
from urllib.parse import urlsplit


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--official-catalog", type=Path, required=True)
    parser.add_argument("--codex-home", type=Path, required=True)
    parser.add_argument("--base-url", required=True)
    args = parser.parse_args()
    url = urlsplit(args.base_url)
    if url.scheme not in ("http", "https") or not url.hostname or url.username or url.password or url.query or url.fragment:
        parser.error("base URL must be an HTTP(S) origin/path without credentials, query or fragment")
    home = args.codex_home.expanduser().resolve()
    if home.exists():
        parser.error("choose a new Codex home; existing files are never overwritten")
    catalog = json.loads(args.official_catalog.read_text())
    model = next(m.copy() for m in catalog["models"] if m["slug"] == "deepseek-flash")
    model.update(
        slug="DeepSeek-V4.1-Flash", display_name="DeepSeek-V4.1-Flash (local)",
        description="Local DeepSeek V4.1 Flash with Recipe.",
        input_modalities=["text", "image"], supports_image_detail_original=False,
        context_window=294912, max_context_window=294912,
        auto_compact_token_limit=240000, default_reasoning_level="max",
        supports_search_tool=False, prefer_websockets=False,
    )
    # JSON basic strings are valid TOML basic strings for these paths/URLs.
    config = f'''model = "DeepSeek-V4.1-Flash"
model_provider = "deepseek_local"
forced_login_method = "api"
model_reasoning_effort = "max"
model_context_window = 294912
model_auto_compact_token_limit = 240000
web_search = "disabled"
show_raw_agent_reasoning = true
model_catalog_json = {json.dumps(str(home / "models.json"))}

[model_providers.deepseek_local]
name = "DeepSeek local"
base_url = {json.dumps(args.base_url.rstrip('/'))}
wire_api = "responses"
env_key = "DEEPSEEK_API_KEY"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 600000

[desktop]
enabled-reasoning-efforts = ["low", "high", "max"]
'''
    home.mkdir(mode=0o700, parents=True)
    for name, content in (("config.toml", config), ("models.json", json.dumps({"models": [model]}, ensure_ascii=False, indent=2) + "\n")):
        fd = os.open(home / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as file:
            file.write(content)
    print(f"Created {home}. Supply DEEPSEEK_API_KEY through a protected Mira account environment file.")


if __name__ == "__main__":
    main()
