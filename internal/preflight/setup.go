package preflight

// LinuxSetup installs sbxr and its requirements on Ubuntu 24.04 with Homebrew on Linux.
// The README holds the same block, and a test keeps both in sync.
// Subordinate ids start after the highest range in each file, so they never overlap another user's range.
const LinuxSetup = `sudo apt-get update && sudo apt-get install -y uidmap
for f in /etc/subuid /etc/subgid; do grep -q "^$USER:" "$f" || echo "$USER:$(awk -F: '{e=$2+$3} e>m{m=e} END{print (m<100000)?100000:m}' "$f"):65536" | sudo tee -a "$f" >/dev/null; done
sudo usermod -aG kvm "$USER"
brew install libkrun
curl -fsSL "https://github.com/albertsko/sbxr/releases/latest/download/sbxr_linux_$(dpkg --print-architecture).tar.gz" | tar -xz sbxr
sudo install -m 755 sbxr /usr/local/bin/sbxr && rm sbxr
sbxr doctor apparmor-profile | sudo tee /etc/apparmor.d/sbxr >/dev/null
sudo apparmor_parser -r /etc/apparmor.d/sbxr
# Log in again so that the kvm group applies, then run: sbxr doctor
`
