#!/bin/bash
set -eux

curl -fsSL https://deno.land/install.sh | sh
cp /root/.deno/bin/deno /usr/local/bin/deno
chmod 755 /usr/local/bin/deno
deno --version
