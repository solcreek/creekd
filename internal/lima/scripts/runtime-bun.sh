#!/bin/bash
set -eux

curl -fsSL https://bun.sh/install | bash
cp /root/.bun/bin/bun /usr/local/bin/bun
chmod 755 /usr/local/bin/bun
bun --version
