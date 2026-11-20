//go:build linux

// Package shim is containerd-shim-sbxr-v1, the sbxr containerd runtime.
//
// containerd uses the shim as the pod sandboxer: one shim process serves the sandbox API for one pod and the task API
// for the pod's containers. The shim starts the pod VM as a child process without host capabilities and forwards each
// task request to the guest agent over vsock. It never imports containerd itself, only its API types.
package shim
