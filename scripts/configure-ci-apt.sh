#!/usr/bin/env bash
set -euo pipefail

# Hosted Ubuntu mirror selection can stall on the Azure HTTP mirror. Keep
# dependency installation on official HTTPS mirrors with bounded retries.
if [[ -f /etc/apt/apt-mirrors.txt ]]; then
  printf '%s\n' 'https://archive.ubuntu.com/ubuntu/' 'https://security.ubuntu.com/ubuntu/' |
    sudo tee /etc/apt/apt-mirrors.txt >/dev/null
fi
printf '%s\n' 'Acquire::http::Timeout "30";' 'Acquire::https::Timeout "30";' 'Acquire::Retries "2";' |
  sudo tee /etc/apt/apt.conf.d/99-mira-ci-network >/dev/null
