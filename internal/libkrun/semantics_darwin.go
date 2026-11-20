package libkrun

// volumeSemantics is KRUN_SEMANTICS_LINUX_SIMPLIFIED.
// The guest sees the uid and gid of the process that asks as the owner, so any guest user can use its files.
const volumeSemantics = 1
