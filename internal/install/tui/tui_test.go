package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/grunclepug/arcus/internal/install"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/profile"
)

func env(t *testing.T) Env {
	return Env{
		UEFI:     true,
		Hardware: install.Hardware{GPUs: []string{"0x1002"}},
		Disks: []install.DiskChoice{
			{Disk: install.Disk{Path: "/dev/sda", Size: 16 << 30, Model: "USB"}, Problem: "/dev/sda is the live medium this system booted from"},
			{Disk: install.Disk{Path: "/dev/nvme0n1", Size: 512 << 30, Model: "SSD"}},
		},
		Timezones: []string{"America/Vancouver", "Europe/Berlin", "UTC"},
		Locales:   []string{"de_DE.UTF-8", "en_US.UTF-8"},
		Keymaps:   []string{"de", "us"},
		Modules: func(p *install.Plan) (module.Set, profile.Set, error) {
			return install.Modules(context.Background(), p, install.MachineFacts(p, true, t.TempDir()))
		},
		Save: func(*install.Plan) (string, error) { return "/root/plan.toml", nil },
	}
}

// send delivers a message and runs the commands it returns, feeding the
// TUI's own messages back, as the program loop would.
func send(m *model, msg tea.Msg) {
	_, cmd := m.Update(msg)
	run(m, cmd)
}

// run runs cmd, giving up on the slow ones: those are cursor blink timers.
// On the loading page it waits, however slow the machine, for the modules.
func run(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	wait := 20 * time.Millisecond
	if m.pages[m.at].loading {
		wait = time.Minute
	}
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(wait):
		return
	}
	switch msg := msg.(type) {
	case loadedMsg, savedMsg:
		send(m, msg)
	case tea.BatchMsg:
		for _, c := range msg {
			run(m, c)
		}
	}
}

func key(m *model, names ...string) {
	codes := map[string]rune{"enter": tea.KeyEnter, "esc": tea.KeyEscape, "down": tea.KeyDown, "up": tea.KeyUp, "space": tea.KeySpace, "tab": tea.KeyTab}
	for _, name := range names {
		if code, ok := codes[name]; ok {
			send(m, tea.KeyPressMsg{Code: code})
		} else {
			for _, r := range name {
				send(m, tea.KeyPressMsg{Code: r, Text: string(r)})
			}
		}
	}
}

func screen(m *model) string { return m.View().Content }

// Walking through with the defaults gives a valid plan: the usable disk,
// the detected hardware, and the secrets kept out of it.
func TestDefaults(t *testing.T) {
	m := newModel(env(t), Defaults())
	key(m, "enter") // welcome
	if !strings.Contains(screen(m), "live medium") {
		t.Errorf("the live medium must be shown as unusable:\n%s", screen(m))
	}
	key(m, "enter", "enter", "enter")          // disk, filesystem, encryption
	key(m, "enter")                            // swap
	key(m, "enter")                            // host name
	key(m, "Berl", "enter")                    // time zone, searched
	key(m, "enter", "enter", "enter", "enter") // locale, keymap, kernel, multilib
	key(m, "me", "tab", "pw", "tab", "px", "enter")
	if !strings.Contains(screen(m), "the two entries differ") {
		t.Fatalf("mismatched passwords must be refused:\n%s", screen(m))
	}
	for range 2 {
		send(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	key(m, "pw", "enter")
	key(m, "enter") // dotfiles: none -> starter Profiles screen
	if !strings.Contains(screen(m), "Profiles") || !strings.Contains(screen(m), "[ ] desktop") {
		t.Fatalf("profiles screen:\n%s", screen(m))
	}
	key(m, "enter") // leave desktop unchecked -> Modules screen
	if !strings.Contains(screen(m), "[x] machine") || !strings.Contains(screen(m), "(always)") {
		t.Fatalf("modules screen:\n%s", screen(m))
	}
	key(m, "enter", "r")

	p, r := m.result.Plan, m.result
	if !r.Run || r.Password != "pw" || p.Disk.Device != "/dev/nvme0n1" || p.System.Timezone != "Europe/Berlin" || p.User.Name != "me" {
		t.Errorf("result = %+v, plan = %+v", r, p)
	}
	if len(p.Host.Profiles) != 0 {
		t.Errorf("profiles = %v, want empty", p.Host.Profiles)
	}
	if want := []string{"base", "btrfs-snapshots"}; !slices.Equal(p.Host.Modules, want) {
		t.Errorf("modules = %v, want %v", p.Host.Modules, want)
	}
	if err := p.Validate(); err != nil {
		t.Error(err)
	}
}

// Encryption asks for the passphrase and rules out a swap partition; going
// back from Profiles passes over the loading screen.
func TestEncryptionAndBack(t *testing.T) {
	m := newModel(env(t), Defaults())
	key(m, "enter", "enter", "enter", "down", "enter") // encryption: yes
	key(m, "secret", "tab", "secret", "enter")
	if !strings.Contains(screen(m), "not with encryption yet") {
		t.Fatalf("swap screen:\n%s", screen(m))
	}
	if m.result.Passphrase != "secret" || !m.plan.Disk.Encrypt {
		t.Errorf("passphrase %q, encrypt %v", m.result.Passphrase, m.plan.Disk.Encrypt)
	}
	key(m, "enter", "enter", "enter", "enter", "enter", "enter", "enter")
	key(m, "me", "tab", "pw", "tab", "pw", "enter", "enter")
	if !strings.Contains(screen(m), "Profiles") {
		t.Fatalf("expected the profiles screen:\n%s", screen(m))
	}
	key(m, "esc")
	if !strings.Contains(screen(m), "Dotfiles") {
		t.Errorf("esc from profiles must go back to the dotfiles screen:\n%s", screen(m))
	}
}

// Selecting the starter desktop profile locks its modules and leaves base
// intact if the user backs out; an explicit opt-out of base survives a round
// trip through desktop.
func TestStarterProfileBackNavigation(t *testing.T) {
	advanceToProfiles := func(t *testing.T) *model {
		t.Helper()
		m := newModel(env(t), Defaults())
		key(m, "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter")
		key(m, "me", "tab", "pw", "tab", "pw", "enter", "enter")
		if !strings.Contains(screen(m), "Profiles") {
			t.Fatalf("expected Profiles screen:\n%s", screen(m))
		}
		return m
	}

	// Select desktop on the very first visit, submit Modules, back out and uncheck desktop -> base returns.
	m := advanceToProfiles(t)
	key(m, "space", "enter") // pick desktop
	if !strings.Contains(screen(m), "(profile)") {
		t.Fatalf("expected profile-fixed modules:\n%s", screen(m))
	}
	key(m, "enter") // submit Modules -> Review
	if !slices.Equal(m.plan.Host.Profiles, []string{"desktop"}) || !slices.Equal(m.plan.Host.Modules, []string{"btrfs-snapshots"}) {
		t.Fatalf("after picking desktop: host = %+v", m.plan.Host)
	}
	key(m, "esc", "esc", "space", "enter", "enter") // Review -> Modules -> Profiles, uncheck desktop, submit Modules
	if len(m.plan.Host.Profiles) != 0 || !slices.Equal(m.plan.Host.Modules, []string{"base", "btrfs-snapshots"}) {
		t.Fatalf("after unchecking desktop: host = %+v, want base restored", m.plan.Host)
	}

	// Explicitly uncheck base first; toggling desktop on and back off keeps base unchecked.
	m2 := advanceToProfiles(t)
	key(m2, "enter")          // skip desktop -> Modules (cursor starts on "base", first item of system)
	key(m2, "space", "enter") // uncheck base, submit Modules -> Review
	if !slices.Equal(m2.plan.Host.Modules, []string{"btrfs-snapshots"}) {
		t.Fatalf("after unchecking base: host = %+v", m2.plan.Host)
	}
	key(m2, "esc", "esc", "space", "enter", "enter") // check desktop, submit Modules
	key(m2, "esc", "esc", "space", "enter", "enter") // uncheck desktop, submit Modules
	if len(m2.plan.Host.Profiles) != 0 || !slices.Equal(m2.plan.Host.Modules, []string{"btrfs-snapshots"}) {
		t.Fatalf("explicit base opt-out did not survive desktop toggle: host = %+v", m2.plan.Host)
	}
}

// A dotfiles repository's profiles come first; their modules are fixed in
// the modules screen and left out of the host file's own modules.
func TestDotfiles(t *testing.T) {
	e := env(t)
	shell, dev := &module.Module{Name: "shell"}, &module.Module{Name: "dev"}
	shell.Category, dev.Category = "system", "dev"
	e.Modules = func(*install.Plan) (module.Set, profile.Set, error) {
		return module.Set{"shell": shell, "dev": dev}, profile.Set{"laptop": {Name: "laptop", Modules: []string{"shell"}}}, nil
	}
	m := newModel(e, Defaults())
	key(m, "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter")
	key(m, "me", "tab", "pw", "tab", "pw", "enter")
	key(m, "https://example.com/dot", "enter", "enter", "enter")
	key(m, "space", "enter") // the laptop profile
	if !strings.Contains(screen(m), "(profile)") {
		t.Fatalf("modules screen:\n%s", screen(m))
	}
	key(m, "down", "space", "enter") // also dev
	if p := m.result.Plan; !slices.Equal(p.Host.Profiles, []string{"laptop"}) || !slices.Equal(p.Host.Modules, []string{"dev"}) {
		t.Errorf("host = %+v", p.Host)
	}

	e.Modules = func(*install.Plan) (module.Set, profile.Set, error) { return nil, nil, errors.New("cloning failed") }
	m = newModel(e, Defaults())
	key(m, "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter")
	key(m, "me", "tab", "pw", "tab", "pw", "enter")
	key(m, "https://example.com/dot", "enter", "enter", "enter")
	if !strings.Contains(screen(m), "cloning failed") {
		t.Errorf("a failed clone must be shown:\n%s", screen(m))
	}
}
