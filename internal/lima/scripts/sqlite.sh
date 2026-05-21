#!/bin/bash
set -eux
export DEBIAN_FRONTEND=noninteractive

apt-get install -y --no-install-recommends sqlite3
sqlite3 --version
