#!/bin/bash
set -eux
export DEBIAN_FRONTEND=noninteractive

apt-get install -y --no-install-recommends redis-server
systemctl enable redis-server
systemctl start redis-server
redis-cli PING
