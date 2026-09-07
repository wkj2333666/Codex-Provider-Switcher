#!/usr/bin/env bash
set -euo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repository="$(cd -- "$script_dir/.." && pwd -P)"
install_root="${HOME}/.local/lib/codex-provider-switcher"
unit_root="${HOME}/.config/systemd/user"
command -v zstd >/dev/null
command -v python3 >/dev/null
command -v systemctl >/dev/null
python3 -c 'import tomllib'
"$script_dir/deploy-local.sh" --check-source
expected_commit="$(git -C "$repository" rev-parse HEAD)"
"$install_root/codex-provider-switcher" --build-info | python3 -c \
 'import json,sys; assert json.load(sys.stdin)["commit"]==sys.argv[1], "deploy the matching switcher first"' "$expected_commit"
install -d -m 0755 "$install_root" "$unit_root"
install -m 0755 "$script_dir/history-maintenance.py" "$install_root/history-maintenance.py"
cat > "$unit_root/codex-history-maintenance.service" <<'UNIT'
[Unit]
Description=Compress cold archived Codex histories

[Service]
Type=oneshot
ExecStart=%h/.local/lib/codex-provider-switcher/history-maintenance.py --home %h/.codex --apply
Nice=15
IOSchedulingClass=idle
CPUQuota=25%
MemoryMax=256M
TimeoutStartSec=10min
UMask=0077
NoNewPrivileges=true
UNIT
cat > "$unit_root/codex-history-maintenance.timer" <<'UNIT'
[Unit]
Description=Daily cold Codex history maintenance

[Timer]
OnCalendar=*-*-* 04:10:00
RandomizedDelaySec=15min
Persistent=true
Unit=codex-history-maintenance.service

[Install]
WantedBy=timers.target
UNIT
"$install_root/history-maintenance.py" --home "$HOME/.codex" --enable
systemctl --user daemon-reload
systemctl --user enable --now codex-history-maintenance.timer
systemctl --user start codex-history-maintenance.service
systemctl --user show codex-history-maintenance.timer --property=ActiveState --property=NextElapseUSecRealtime
