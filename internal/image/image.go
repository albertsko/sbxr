// Package image pulls OCI images, unpacks them into a local cache, and prepares per-run roots.
package image

// Image is an unpacked image in the cache. Its files must not change.
type Image struct {
	// Root is the cached root directory.
	Root string
	// Config is the image run configuration.
	Config Config
}

// Config is the part of an OCI image config that shapes the workload.
type Config struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
}
