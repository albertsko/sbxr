package preflight

// AppArmorProfile lets the root-owned sbxr install paths create user namespaces on Ubuntu 23.10 and later.
// It never covers a user-writable path, because that would let any local user bypass the restriction.
const AppArmorProfile = `abi <abi/4.0>,
include <tunables/global>

profile sbxr /{usr/bin,usr/local/bin}/sbxr flags=(unconfined) {
  userns,

  include if exists <local/sbxr>
}
`
