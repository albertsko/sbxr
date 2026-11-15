#!/usr/bin/env bash
# Collects crun, libkrun, libkrunfw, and their non-glibc libraries into one tree at <prefix>.
# Usage: collect.sh <prefix>
# The tree works only at <prefix> (for example /opt/sbxr), because crun's RUNPATH is absolute.
set -euo pipefail

prefix=$1
mkdir -p "$prefix/bin" "$prefix/lib"

install -m 755 /src/crun/crun "$prefix/bin/crun"
strip --strip-unneeded "$prefix/bin/crun"
ln -s crun "$prefix/bin/krun"
cp -P /usr/local/lib/libkrun.so.1* "$prefix/lib/"
find /src/fw -name 'libkrunfw.so.5*' -exec cp -P {} "$prefix/lib/" \;

# crun opens libkrun with dlopen, and libkrun opens libkrunfw the same way, so ldd sees neither link.
# glibc stays on the node: every distribution has it, and mixing two copies breaks the process.
glibc='^(ld-linux.*|libc|libm|libdl|libpthread|librt|libresolv|libutil|libanl)\.so'
ldd "$prefix/bin/crun" "$prefix/lib/libkrun.so.1" |
  awk '/=> \// {print $1, $3}' |
  sort -u |
  while read -r soname path; do
    if [[ $soname =~ $glibc || -e $prefix/lib/$soname ]]; then
      continue
    fi
    cp -L "$path" "$prefix/lib/$soname"
  done

# crun re-executes itself from a sealed memfd, where $ORIGIN resolves to "/", so crun gets the absolute path.
# Libraries load from real files, so $ORIGIN works for them.
patchelf --set-rpath "$prefix/lib" "$prefix/bin/crun"
find "$prefix/lib" -type f -name '*.so*' -exec patchelf --set-rpath '$ORIGIN' {} \;

leaks=$(ldd "$prefix/bin/crun" "$prefix"/lib/* | grep '=>' | grep -v "=> $prefix/lib/" | grep -v -E '\s(ld-linux.*|libc|libm|libdl|libpthread|librt|libresolv|libutil|libanl)\.so' || true)
if [[ -n $leaks ]]; then
  echo "libraries outside the bundle: $leaks" >&2
  exit 1
fi
"$prefix/bin/krun" --version
echo "glibc floor: $(objdump -T "$prefix"/bin/crun "$prefix"/lib/*.so* | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1)"
ls -lA "$prefix/bin" "$prefix/lib"
du -sh "$prefix"
