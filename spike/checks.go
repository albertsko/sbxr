//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var failed int

func check(name string, ok bool, detail string) {
	status := "PASS"
	if !ok {
		status = "FAIL"
		failed++
	}
	fmt.Printf("CHECK %s %s: %s\n", status, name, strings.TrimSpace(detail))
}

func sh(script string) (string, error) {
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	return string(out), err
}

// vm boots one microVM in a child process and returns its output and exit code.
func vm(root, script string) (string, int) {
	exe, _ := os.Executable()
	var buf bytes.Buffer
	cmd := exec.Command(exe, "vm", root, "/bin/sh", "-c", script)
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return buf.String(), code
}

func manifest(dir string) string {
	out, err := sh(fmt.Sprintf(`cd %q && { find . -printf '%%p %%U:%%G %%m %%y %%s %%T@\n' | sort; find . -type f -print0 | sort -z | xargs -0 sha256sum; } | sha256sum`, dir))
	if err != nil {
		return "error: " + out
	}
	return out
}

func mountOverlay(lower, run string) (string, time.Duration, error) {
	upper, work, merged := filepath.Join(run, "upper"), filepath.Join(run, "work"), filepath.Join(run, "merged")
	for _, d := range []string{upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", 0, err
		}
	}
	start := time.Now()
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,userxattr", lower, upper, work)
	if err := unix.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return "", 0, fmt.Errorf("mount overlay %s: %w", opts, err)
	}
	return merged, time.Since(start), nil
}

func runChecks(args []string) error {
	work, alpineTar, nodeTar := args[0], args[1], args[2]
	fmt.Printf("ns ids: uid=%d gid=%d\n", os.Getuid(), os.Getgid())
	out, _ := sh("cat /proc/self/uid_map /proc/self/gid_map; grep Cap /proc/self/status; uname -r")
	fmt.Print(out)

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	cache := filepath.Join(work, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	if out, err := sh(fmt.Sprintf("tar -xzf %q -C %q", alpineTar, cache)); err != nil {
		return fmt.Errorf("unpack alpine: %v: %s", err, out)
	}
	out, _ = sh(fmt.Sprintf("stat -c '%%n %%u:%%g %%a' %q/etc/shadow; ls -ln %q/etc/shadow", cache, cache))
	fmt.Print("cache ns view: ", out)
	before := manifest(cache)

	// Run 1: ownership, writes, deletes, exit code.
	merged1, d, err := mountOverlay(cache, filepath.Join(work, "run1"))
	if err != nil {
		return err
	}
	fmt.Printf("overlay mount time: %v\n", d)

	script := `set -e
stat -c 'shadow=%u:%g %a' /etc/shadow
ls -ln /.krun_config.json 2>&1 || true
echo guest > /etc/guest-write
echo extra >> /etc/motd
rm /etc/hostname
rm -rf /etc/apk && mkdir /etc/apk && echo new > /etc/apk/only
mkdir /newdir && touch /newdir/f && chown 1000:1000 /newdir/f
test ! -e /etc/hostname && echo hostname-gone
ls /etc/apk
exit 7`
	out, code := vm(merged1, script)
	fmt.Print("--- run1 guest output\n", out, "---\n")
	check("guest-ownership-shadow-0:42", strings.Contains(out, "shadow=0:42 640"), firstLine(out, "shadow="))
	check("exit-code-passthrough-7", code == 7, fmt.Sprintf("host exit=%d", code))
	check("guest-sees-delete", strings.Contains(out, "hostname-gone"), "")

	for _, c := range []int{0, 3} {
		_, code := vm(merged1, fmt.Sprintf("exit %d", c))
		check(fmt.Sprintf("exit-code-passthrough-%d", c), code == c, fmt.Sprintf("host exit=%d", code))
	}

	out, code = vm(merged1, `test ! -e /etc/hostname && cat /etc/guest-write && ls /etc/apk && stat -c 'newfile=%u:%g' /newdir/f`)
	check("persist-in-run-root-on-reboot", code == 0 && strings.Contains(out, "guest") && strings.Contains(out, "only") && strings.Contains(out, "newfile=1000:1000"), oneLine(out))

	upper := filepath.Join(work, "run1", "upper")
	out, _ = sh(fmt.Sprintf(`cd %q && find . -printf '%%p %%U:%%G %%y\n' | sort; stat -c 'hostname: %%F %%t:%%T' etc/hostname`, upper))
	fmt.Print("--- upper contents\n", out, "---\n")
	check("write-lands-in-upper", strings.Contains(out, "./etc/guest-write 0:0 f") && strings.Contains(out, "./etc/motd"), "")
	check("whiteout-char-0:0", strings.Contains(out, "hostname: character special file 0:0"), firstLine(out, "hostname:"))
	opaque, err := getxattr(filepath.Join(upper, "etc/apk"), "user.overlay.opaque")
	check("opaque-dir-user.overlay.opaque", err == nil && opaque == "y", fmt.Sprintf("value=%q err=%v", opaque, err))
	check("krun-config-not-in-upper", !strings.Contains(out, "krun_config"), "")

	after := manifest(cache)
	check("cache-unchanged-run1", before == after, fmt.Sprintf("before=%s after=%s", oneLine(before), oneLine(after)))
	out, _ = sh(fmt.Sprintf("ls -la %q/etc/hostname %q/etc/guest-write 2>&1", cache, cache))
	fmt.Print("cache view: ", out)

	// Concurrent runs on the same lower.
	merged2, _, err := mountOverlay(cache, filepath.Join(work, "run2"))
	if err != nil {
		return err
	}
	merged3, _, err := mountOverlay(cache, filepath.Join(work, "run3"))
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	res := make([]string, 2)
	codes := make([]int, 2)
	for i, m := range []string{merged2, merged3} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			me, other := fmt.Sprintf("r%d", i+2), fmt.Sprintf("r%d", 3-i)
			s := fmt.Sprintf(`echo %s > /etc/whoami; rm /etc/os-release; sleep 3; echo "seen=$(cat /etc/whoami)"; test -e /etc/mark-%s && echo LEAK; touch /etc/mark-%s; sleep 2; test -e /etc/mark-%s && echo LEAK; test -e /etc/os-release && echo LEAK-DELETE; exit %d`, me, other, me, other, 20+i)
			res[i], codes[i] = vm(m, s)
		}()
	}
	wg.Wait()
	fmt.Printf("--- concurrent\nr2(exit %d): %s\nr3(exit %d): %s\n---\n", codes[0], oneLine(res[0]), codes[1], oneLine(res[1]))
	check("concurrent-isolated", strings.Contains(res[0], "seen=r2") && strings.Contains(res[1], "seen=r3") && !strings.Contains(res[0]+res[1], "LEAK"), "")
	check("concurrent-exit-codes", codes[0] == 20 && codes[1] == 21, fmt.Sprintf("%d %d", codes[0], codes[1]))
	check("cache-unchanged-concurrent", manifest(cache) == before, "")

	for _, m := range []string{merged1, merged2, merged3} {
		if err := unix.Unmount(m, 0); err != nil {
			check("unmount", false, err.Error())
		}
	}

	// Fallback copy timing on a larger tree.
	node := filepath.Join(work, "node")
	os.MkdirAll(node, 0o755)
	if out, err := sh(fmt.Sprintf("tar -xf %q -C %q", nodeTar, node)); err != nil {
		fmt.Print("node unpack warnings: ", out)
	}
	out, _ = sh(fmt.Sprintf("du -sh %q; find %q | wc -l; df -T %q | tail -1", node, node, work))
	fmt.Print("node tree: ", out)
	for i := 1; i <= 3; i++ {
		sh("sync")
		start := time.Now()
		dst := filepath.Join(work, fmt.Sprintf("nodecopy%d", i))
		out, err := sh(fmt.Sprintf("cp -a --reflink=auto %q %q", node, dst))
		dur := time.Since(start)
		check(fmt.Sprintf("cp-reflink-auto-%d", i), err == nil, fmt.Sprintf("time=%v %s", dur, oneLine(out)))
	}
	out, _ = sh(fmt.Sprintf("stat -c '%%n %%u:%%g %%a' %q/etc/shadow %q/etc/shadow", node, filepath.Join(work, "nodecopy1")))
	fmt.Print("copy ownership: ", out)
	mn, d, err := mountOverlay(node, filepath.Join(work, "noderun"))
	if err == nil {
		fmt.Printf("node overlay mount time: %v\n", d)
		out, code := vm(mn, `node -e 'console.log(process.version)'; stat -c 'shadow=%u:%g %a' /etc/shadow; exit 5`)
		check("node-on-overlay", code == 5 && strings.Contains(out, "v22"), oneLine(out))
		unix.Unmount(mn, 0)
	}

	fmt.Printf("FAILED CHECKS: %d\n", failed)
	if failed > 0 {
		os.Exit(1)
	}
	return nil
}

func getxattr(path, name string) (string, error) {
	buf := make([]byte, 256)
	n, err := unix.Lgetxattr(path, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func firstLine(s, prefix string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, prefix) {
			return l
		}
	}
	return ""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
