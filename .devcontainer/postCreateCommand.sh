#!/bin/sh
cd /tmp

wget https://go.dev/dl/go1.25.0.linux-arm64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.0.linux-arm64.tar.gz
rm go1.25.0.linux-arm64.tar.gz
echo "export PATH=\$PATH:/usr/local/go/bin" >> ~/.profile
source  ~/.profile

wget https://github.com/tinygo-org/tinygo/releases/download/v0.39.0/tinygo_0.39.0_arm64.deb
sudo dpkg -i tinygo_0.39.0_arm64.deb
rm tinygo_0.39.0_arm64.deb

exit 0
