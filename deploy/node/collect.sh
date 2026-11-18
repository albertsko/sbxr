#!/usr/bin/env bash
# Collects libkrun, libkrunfw, and their non-glibc libraries into one tree at <prefix>.
# Usage: collect.sh <prefix>
set -euo pipefail

prefix=$1
licenses=$prefix/share/licenses
mkdir -p "$prefix/bin" "$prefix/lib" "$licenses"

cp -P /usr/local/lib/libkrun.so.1* "$prefix/lib/"
find /src/fw -name 'libkrunfw.so.5*' -exec cp -P {} "$prefix/lib/" \;

# The shim opens libkrun with dlopen, and libkrun opens libkrunfw the same way, so ldd sees neither link.
# glibc stays on the node: every distribution has it, and mixing two copies breaks the process.
glibc='^(ld-linux.*|libc|libm|libdl|libpthread|librt|libresolv|libutil|libanl)\.so'
ldd "$prefix/lib/libkrun.so.1" |
	awk '/=> \// {print $1, $3}' |
	sort -u |
	while read -r soname path; do
		if [[ $soname =~ $glibc || -e $prefix/lib/$soname ]]; then
			continue
		fi
		cp -L "$path" "$prefix/lib/$soname"
		echo "$path"
	done >/tmp/copied

# The Ubuntu packages of the copied libraries. Ubuntu merges /lib into /usr/lib, and dpkg knows each file under one of the two.
packages=()
while read -r path; do
	pkg=$( (dpkg -S "$path" || dpkg -S "/usr$path" || dpkg -S "${path#/usr}") 2>/dev/null | head -1)
	packages+=("${pkg%%[:,]*}")
done </tmp/copied
mapfile -t packages < <(printf '%s\n' "${packages[@]}" | sort -u)

# License texts and a NOTICE with versions, sources, and changes, for the GPL, LGPL, and Apache-2.0 terms.
install -D -m 644 -t "$licenses/crun" /src/crun/COPYING /src/crun/COPYING.libcrun
install -D -m 644 -t "$licenses/libkrun" /src/libkrun/LICENSE
install -D -m 644 -t "$licenses/libkrunfw" /src/libkrunfw/LICENSE-GPL-2.0-only /src/libkrunfw/LICENSE-LGPL-2.1-only
for pkg in "${packages[@]}"; do
	install -D -m 644 "/usr/share/doc/$pkg/copyright" "$licenses/$pkg/copyright"
done
{
	echo "sbxr node bundle. License texts are in share/licenses/<component>."
	echo
	echo "crun $CRUN_VERSION, in the guest root (GPL-2.0-or-later, libcrun LGPL-2.1-or-later): https://github.com/containers/crun/releases/tag/$CRUN_VERSION"
	echo "libkrun $LIBKRUN_VERSION, commit $LIBKRUN_COMMIT (Apache-2.0): https://github.com/libkrun/libkrun/tree/$LIBKRUN_COMMIT"
	echo "  Changed by sbxr: in src/init_blob/init/init.c, config_parse_env calls setenv with overwrite 1 for every entry, so the last of duplicate env entries wins."
	echo "libkrunfw $LIBKRUNFW_VERSION, commit $LIBKRUNFW_COMMIT (LGPL-2.1-only, the bundled Linux kernel GPL-2.0-only): https://github.com/libkrun/libkrunfw/tree/$LIBKRUNFW_COMMIT"
	echo "  Kernel source: the tarball that the libkrunfw Makefile names at that commit, from https://github.com/libkrun/linux"
	echo
	echo "Libraries from Ubuntu 22.04 packages (source: apt-get source <package>=<version>):"
	for pkg in "${packages[@]}"; do
		echo "$pkg $(dpkg-query -W -f '${Version}' "$pkg")"
	done
} >"$prefix/NOTICE"

# shellcheck disable=SC2016 # $ORIGIN is for the dynamic linker.
find "$prefix/lib" -type f -name '*.so*' -exec patchelf --set-rpath '$ORIGIN' {} \;

leaks=$(ldd "$prefix"/lib/* | grep '=>' | grep -v "=> $prefix/lib/" | grep -v -E '\s(ld-linux.*|libc|libm|libdl|libpthread|librt|libresolv|libutil|libanl)\.so' || true)
if [[ -n $leaks ]]; then
	echo "libraries outside the bundle: $leaks" >&2
	exit 1
fi
echo "glibc floor: $(objdump -T "$prefix"/lib/*.so* | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1)"
cat "$prefix/NOTICE"
ls -lA "$prefix/bin" "$prefix/lib" "$licenses"
