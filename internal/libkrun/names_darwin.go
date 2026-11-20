package libkrun

const (
	libkrunName = "libkrun.1.dylib"
	krunfwName  = "libkrunfw.5.dylib"
	libPathVar  = "DYLD_FALLBACK_LIBRARY_PATH"
)

func defaultLibDirs() []string {
	return []string{"/opt/homebrew/lib", "/usr/local/lib"}
}
