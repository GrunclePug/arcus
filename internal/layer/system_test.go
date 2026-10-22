package layer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// System files are regular-file copies (never symlinks into the user-writable
// module), with the recorded hash, a fixed mode and the deploying owner.
func TestSystemCopiesRecordHashModeOwner(t *testing.T) {
	sb := newSandbox(t, halves[1])
	conf := sb.moduleFile("p", "sysctl.d/90-p.conf", "vm.swappiness = 10\n")
	must(t, os.Chmod(conf, 0o666)) // group/other write must not reach /etc
	script := sb.moduleFile("p", "script", "#!/bin/sh\n")
	must(t, os.Chmod(script, 0o775))
	must(t, sb.activate("p"))

	for rel, mode := range map[string]os.FileMode{"sysctl.d/90-p.conf": 0o644, "script": 0o755} {
		info, err := os.Lstat(filepath.Join(sb.host, rel))
		must(t, err)
		if !info.Mode().IsRegular() {
			t.Errorf("%s: deployed as %v, want a regular file", rel, info.Mode().Type())
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s: mode %v, want %v", rel, info.Mode().Perm(), mode)
		}
	}

	l, err := sb.s.LoadLedger("p")
	must(t, err)
	for _, e := range l.Entries {
		sum, err := hashFile(e.Host)
		must(t, err)
		if e.Kind != KindCopy || e.SHA256 != sum || e.UID != os.Geteuid() || e.GID != os.Getegid() {
			t.Errorf("ledger entry %+v does not describe the deployed file (sha256 %s)", e, sum)
		}
	}
	if findings, _ := sb.s.Audit("p", sb.stack); len(findings) != 0 {
		t.Fatalf("fresh layer has findings: %v", findings)
	}

	// Editing the module does not change the deployed copy; audit reports
	// it so apply can redeploy. A chmod on the host is drift too.
	must(t, os.WriteFile(conf, []byte("vm.swappiness = 60\n"), 0o644))
	must(t, os.Chmod(filepath.Join(sb.host, "script"), 0o700))
	findings, err := sb.s.Audit("p", sb.stack)
	must(t, err)
	got := strings.Join(findings, "\n")
	if len(findings) != 2 || !strings.Contains(got, "changed since deployment") || !strings.Contains(got, "mode/owner is 0700") {
		t.Fatalf("findings = %v", findings)
	}
	if sb.read("sysctl.d/90-p.conf") != "vm.swappiness = 10\n" {
		t.Error("deployed copy must not follow the module")
	}
}

func TestSystemRejectsPathsOutsideAllowedDirs(t *testing.T) {
	sb := newSandbox(t, halves[1])
	dir := sb.module("p")
	for _, rel := range []string{"etc/ok.conf", "usr/local/bin/ok", "usr/bin/river", "var/lib/x", "top-level"} {
		p := filepath.Join(dir, "system", rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, nil, 0o644))
	}
	before := sb.snapshot()

	err := sb.s.Activate(context.Background(), "p", nil)
	if err == nil {
		t.Fatal("expected pre-flight failure")
	}
	for _, bad := range []string{"usr/bin/river", "var/lib/x", "top-level"} {
		if !strings.Contains(err.Error(), bad+": outside the allowed directories") {
			t.Errorf("%s not rejected: %v", bad, err)
		}
	}
	for _, good := range []string{"etc/ok.conf", "usr/local/bin/ok"} {
		if strings.Contains(err.Error(), good) {
			t.Errorf("%s rejected: %v", good, err)
		}
	}
	assertSame(t, before, sb.snapshot())
	sb.assertNoLedgers()
}

// A symlink in system/ would make root copy whatever it points to, including
// files the user can't read.
func TestSystemRejectsSymlinks(t *testing.T) {
	sb := newSandbox(t, halves[1])
	sb.moduleFile("p", "ok.conf", "x")
	secret := filepath.Join(sb.root, "secret")
	must(t, os.WriteFile(secret, []byte("root only"), 0o600))
	must(t, os.Symlink(secret, filepath.Join(sb.module("p"), "system", "etc", "leak.conf")))
	before := sb.snapshot()

	err := sb.activate("p")
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("want symlink rejected, got %v", err)
	}
	assertSame(t, before, sb.snapshot())
}

// Each half deploys only its own subdirectory, into its own target, with its
// own ledger.
func TestHomeAndSystemHalvesAreSeparate(t *testing.T) {
	root := t.TempDir()
	dirs := map[string]string{"p": filepath.Join(root, "modules", "p")}
	for _, rel := range []string{"home/.config/foot/foot.ini", "system/etc/foot.conf"} {
		p := filepath.Join(dirs["p"], rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(rel), 0o644))
	}
	home, sys := filepath.Join(root, "home"), filepath.Join(root, "root")
	must(t, os.MkdirAll(home, 0o755))
	must(t, os.MkdirAll(sys, 0o755))
	user := NewHome(home, filepath.Join(root, "user-state"), dirs)
	system := NewSystem(sys, filepath.Join(root, "system-state"), dirs)

	ctx := context.Background()
	must(t, user.Activate(ctx, "p", nil))
	must(t, system.Activate(ctx, "p", nil))

	if target, err := os.Readlink(filepath.Join(home, ".config/foot/foot.ini")); err != nil || target != filepath.Join(dirs["p"], "home/.config/foot/foot.ini") {
		t.Errorf("home file: %q, %v", target, err)
	}
	if info, err := os.Lstat(filepath.Join(sys, "etc/foot.conf")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("system file: %v, %v", info, err)
	}
	for _, stray := range []string{filepath.Join(home, "etc"), filepath.Join(sys, ".config")} {
		if exists(stray) {
			t.Errorf("%s deployed by the wrong half", stray)
		}
	}
	for _, s := range []*Stacker{user, system} {
		l, err := s.LoadLedger("p")
		must(t, err)
		if l == nil || len(l.Entries) != 1 {
			t.Errorf("%s ledger: %+v", s.Subdir, l)
		}
	}
}

func TestStale(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		kept := sb.moduleFile("p", "kept", "k")
		edited := sb.moduleFile("p", "edited", "v1")
		removed := sb.moduleFile("p", "removed", "r")
		must(t, sb.activate("p"))

		changes, err := sb.s.Stale("p")
		must(t, err)
		if len(changes) != 0 {
			t.Fatalf("fresh layer is stale: %v", changes)
		}

		must(t, os.WriteFile(edited, []byte("v2"), 0o644))
		must(t, os.Remove(removed))
		added := sb.moduleFile("p", "dir/added", "a")
		must(t, os.Chmod(kept, 0o755))

		changes, err = sb.s.Stale("p")
		must(t, err)
		got := map[string]byte{}
		for _, c := range changes {
			got[c.Src] = c.Op
		}
		want := map[string]byte{removed: '-', added: '+'}
		if sb.s.Copy {
			want[edited], want[kept] = '~', '~' // links follow their source
		}
		if len(got) != len(want) {
			t.Fatalf("changes = %v, want %v", got, want)
		}
		for src, op := range want {
			if got[src] != op {
				t.Errorf("%s: got %q, want %q", src, got[src], op)
			}
		}
	})
}

// Deploying through a symlinked directory would write wherever it points.
func TestSymlinkedParentIsRefused(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		elsewhere := filepath.Join(sb.root, "dotfiles", "nvim")
		must(t, os.MkdirAll(elsewhere, 0o755))
		must(t, os.WriteFile(filepath.Join(elsewhere, "init.lua"), []byte("mine"), 0o644))
		must(t, os.Symlink(elsewhere, filepath.Join(sb.host, "nvim")))
		sb.moduleFile("p", "nvim/init.lua", "module")

		err := sb.activate("p")
		if err == nil || !strings.Contains(err.Error(), "is a symlink") {
			t.Fatalf("want a symlinked-parent refusal, got %v", err)
		}
		data, _ := os.ReadFile(filepath.Join(elsewhere, "init.lua"))
		if string(data) != "mine" || exists(filepath.Join(elsewhere, "init.lua.orig")) {
			t.Error("nothing may be written through the symlink")
		}
	})
}

func TestPreviewShowsReplacedFiles(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("existing", "host")
		sb.moduleFile("p", "existing", "p")
		sb.moduleFile("p", "new", "p")
		entries, err := sb.s.Preview("p", nil, nil)
		must(t, err)
		if len(entries) != 2 {
			t.Fatalf("entries = %+v", entries)
		}
		for _, e := range entries {
			if replaced := strings.HasSuffix(e.Host, "existing"); replaced != (e.Backup != "") {
				t.Errorf("%s: backup %q", e.Host, e.Backup)
			}
		}
		sb.assertNoLedgers()
	})
}
