package preflight

// LinuxSetup installs sbxr and its requirements on Ubuntu 24.04 with Homebrew on Linux.
// The README holds the same block, and a test keeps both in sync.
const LinuxSetup = `sudo apt-get install -y uidmap
grep -q "^$USER:" /etc/subuid || sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "$USER"
sudo usermod -aG kvm "$USER"
brew install libkrun
curl -fsSL "https://github.com/albertsko/sbxr/releases/latest/download/sbxr_linux_$(dpkg --print-architecture).tar.gz" | tar -xz sbxr
sudo install -m 755 sbxr /usr/local/bin/sbxr && rm sbxr
sbxr doctor apparmor-profile | sudo tee /etc/apparmor.d/sbxr >/dev/null
sudo apparmor_parser -r /etc/apparmor.d/sbxr
# Log in again so that the kvm group applies, then run: sbxr doctor
`
