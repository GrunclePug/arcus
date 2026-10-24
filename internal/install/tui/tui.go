// Package tui is the installer's plan editor.
// It only fills in a plan: running it is install.Run's job, after the TUI
// has exited, so a failure leaves a readable terminal.
package tui

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/grunclepug/arcus/internal/install"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/profile"
)

// Env is everything the TUI reads from the machine, gathered before it
// starts so the screens stay free of I/O. Modules and Save run as commands.
type Env struct {
	UEFI      bool
	Hardware  install.Hardware
	Disks     []install.DiskChoice
	Timezones []string
	Locales   []string
	Keymaps   []string
	// Modules loads what the plan can select: the installer modules, or
	// the dotfiles repository's modules and profiles.
	Modules func(*install.Plan) (module.Set, profile.Set, error)
	// Save writes the plan and returns where.
	Save func(*install.Plan) (string, error)
}

// Result is how the TUI ended.
type Result struct {
	Plan *install.Plan
	Run  bool // the user chose to run the plan now
	// The secrets typed in, for install.Run; never part of the plan.
	Password, Passphrase string
}

// Defaults is the plan the screens start from.
func Defaults() *install.Plan {
	p := &install.Plan{Version: install.Version}
	p.Disk.Filesystem, p.Disk.Swap = "btrfs", "zram"
	p.System.Hostname, p.System.Timezone, p.System.Locale, p.System.Keymap, p.System.Kernel = "archlinux", "UTC", "en_US.UTF-8", "us", "linux"
	p.User.Wheel = true
	p.Dotfiles.Path, p.Dotfiles.Arcus = "~/dotfiles", "config/arcus"
	return p
}

// page builds a screen from the plan so far; skip leaves it out, and a
// loading page is passed over when going back.
type page struct {
	skip    func(*model) bool
	build   func(*model) (step, tea.Cmd)
	loading bool
}

type model struct {
	env      Env
	plan     *install.Plan
	result   Result
	modules  module.Set
	profiles profile.Set
	pages    []page
	at       int
	cur      step
	height   int
}

// loadedMsg carries what Env.Modules loaded.
type loadedMsg struct {
	modules  module.Set
	profiles profile.Set
	err      error
}

// savedMsg reports where Env.Save wrote the plan.
type savedMsg struct {
	path string
	err  error
}

// Run shows the screens and returns how they ended.
func Run(env Env, p *install.Plan) (Result, error) {
	m := newModel(env, p)
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return Result{}, err
	}
	return final.(*model).result, nil
}

func newModel(env Env, p *install.Plan) *model {
	m := &model{env: env, plan: p, result: Result{Plan: p}, height: 24}
	m.pages = pages()
	m.cur, _ = m.pages[0].build(m)
	return m
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
	case loadedMsg:
		if msg.err != nil {
			m.cur = &text{title: "Modules", body: alert.Render(msg.err.Error()) + "\n\n" + faint.Render("esc goes back to change the repository")}
			return m, nil
		}
		m.modules, m.profiles = msg.modules, msg.profiles
		return m, m.next()
	case savedMsg:
		if r, ok := m.cur.(*review); ok {
			r.saved, r.err = msg.path, msg.err
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			return m, m.back()
		}
		done, cmd := m.cur.update(msg)
		if r, ok := m.cur.(*review); ok {
			switch {
			case r.run:
				m.result.Run = true
				return m, tea.Quit
			case r.quit:
				return m, tea.Quit
			case r.save:
				r.save = false
				return m, func() tea.Msg { path, err := m.env.Save(m.plan); return savedMsg{path, err} }
			}
		}
		if done {
			return m, tea.Batch(cmd, m.next())
		}
		return m, cmd
	}
	return m, nil
}

// next moves to the next page that applies.
func (m *model) next() tea.Cmd {
	for i := m.at + 1; i < len(m.pages); i++ {
		if p := m.pages[i]; p.skip == nil || !p.skip(m) {
			m.at = i
			var cmd tea.Cmd
			m.cur, cmd = p.build(m)
			return cmd
		}
	}
	return nil
}

// back moves to the previous page that applies, passing over loading ones.
func (m *model) back() tea.Cmd {
	for i := m.at - 1; i >= 0; i-- {
		if p := m.pages[i]; !p.loading && (p.skip == nil || !p.skip(m)) {
			m.at = i
			var cmd tea.Cmd
			m.cur, cmd = p.build(m)
			return cmd
		}
	}
	return nil
}

func (m *model) View() tea.View {
	help := "enter: next · esc: back · ctrl+c: quit"
	switch m.cur.(type) {
	case *multi:
		help = "space: toggle · " + help
	case *review:
		help = "↑/↓: scroll · s: save · r: run · esc: back · q: quit"
	}
	body := m.cur.view(m.height - 4)
	v := tea.NewView(faint.Render("arcus-install") + "\n\n" + body + "\n" + faint.Render(help))
	v.AltScreen = true
	return v
}

func pages() []page {
	return []page{
		{build: welcome},
		{build: disk},
		{build: func(m *model) (step, tea.Cmd) {
			return newChoice("Filesystem", "btrfs gets snapshots of / (snapper) and a boot menu entry for each.", []option{
				{label: "btrfs, with snapshots", value: "btrfs"}, {label: "ext4", value: "ext4"}, {label: "xfs", value: "xfs"},
			}, m.plan.Disk.Filesystem, func(v string) { m.plan.Disk.Filesystem = v }), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			current := "no"
			if m.plan.Disk.Encrypt {
				current = "yes"
			}
			return newChoice("Encryption", "LUKS2 on the root partition; /boot stays unencrypted.", []option{
				{label: "no", value: "no"}, {label: "yes, ask for a passphrase at boot", value: "yes"},
			}, current, func(v string) { m.plan.Disk.Encrypt = v == "yes" }), nil
		}},
		{skip: func(m *model) bool { return !m.plan.Disk.Encrypt }, build: func(m *model) (step, tea.Cmd) {
			return newForm("Disk passphrase", "Asked at every boot. It is never written to a file.", func(v []string) error {
				if err := same(v[0], v[1]); err != nil {
					return err
				}
				m.result.Passphrase = v[0]
				return nil
			}, newField("passphrase", m.result.Passphrase, true), newField("again", m.result.Passphrase, true)), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			options := []option{{label: "zram: compressed RAM, no disk space", value: "zram"}, {label: "a swap partition (for hibernation)", value: "partition"}, {label: "none", value: "none"}}
			if m.plan.Disk.Encrypt {
				options[1].disabled = "not with encryption yet"
			}
			return newChoice("Swap", "", options, m.plan.Disk.Swap, func(v string) {
				m.plan.Disk.Swap = v
				if v != "partition" {
					m.plan.Disk.SwapSize = ""
				}
			}), nil
		}},
		{skip: func(m *model) bool { return m.plan.Disk.Swap != "partition" }, build: func(m *model) (step, tea.Cmd) {
			return newForm("Swap partition size", "For hibernation, at least the RAM size; for example 16G.", func(v []string) error {
				m.plan.Disk.SwapSize = v[0]
				return m.check("disk.swap_size")
			}, newField("size", m.plan.Disk.SwapSize, false)), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			return newForm("Host name", "This machine's name.", func(v []string) error {
				m.plan.System.Hostname = v[0]
				return m.check("system.hostname")
			}, newField("host name", m.plan.System.Hostname, false)), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			return listOf("Time zone", m.env.Timezones, m.plan.System.Timezone, func(v string) { m.plan.System.Timezone = v }), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			return listOf("Locale", m.env.Locales, m.plan.System.Locale, func(v string) { m.plan.System.Locale = v }), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			return listOf("Console keymap", m.env.Keymaps, m.plan.System.Keymap, func(v string) { m.plan.System.Keymap = v }), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			return newChoice("Kernel", "", []option{
				{label: "linux", value: "linux"}, {label: "linux-lts", value: "linux-lts"}, {label: "linux-zen", value: "linux-zen"},
			}, m.plan.System.Kernel, func(v string) { m.plan.System.Kernel = v }), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			current := "no"
			if m.plan.System.Multilib {
				current = "yes"
			}
			return newChoice("32-bit packages", "The [multilib] repository, which steam and wine need.", []option{
				{label: "no", value: "no"}, {label: "yes, enable [multilib]", value: "yes"},
			}, current, func(v string) { m.plan.System.Multilib = v == "yes" }), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			return newForm("User", "Your login, in the wheel group (sudo); root is locked. The password is never written to a file.", func(v []string) error {
				m.plan.User.Name = v[0]
				if err := m.check("user.name"); err != nil {
					return err
				}
				if err := same(v[1], v[2]); err != nil {
					return err
				}
				m.result.Password = v[1]
				return nil
			}, newField("name", m.plan.User.Name, false), newField("password", m.result.Password, true), newField("again", m.result.Password, true)), nil
		}},
		{build: func(m *model) (step, tea.Cmd) {
			f := newForm("Dotfiles", "A git repository with your arcus config (optional). Empty: the installer's own modules.", func(v []string) error {
				if v[0] != m.plan.Dotfiles.URL {
					m.plan.Host.Profiles, m.plan.Host.Modules = nil, nil
				}
				m.plan.Dotfiles.URL, m.plan.Dotfiles.Path, m.plan.Dotfiles.Arcus = v[0], v[1], v[2]
				return m.check("dotfiles.")
			}, newField("repository URL", m.plan.Dotfiles.URL, false), newField("clone to", m.plan.Dotfiles.Path, false), newField("arcus config in it", m.plan.Dotfiles.Arcus, false))
			f.enough = func(v []string) bool { return v[0] == "" }
			return f, nil
		}},
		{loading: true, build: func(m *model) (step, tea.Cmd) {
			p := *m.plan
			return &text{title: "Modules", body: faint.Render("loading the modules...")}, func() tea.Msg {
				modules, profiles, err := m.env.Modules(&p)
				return loadedMsg{modules, profiles, err}
			}
		}},
		{skip: func(m *model) bool { return len(m.profiles) == 0 }, build: profiles},
		{build: modules},
		{build: func(m *model) (step, tea.Cmd) { return &review{m: m}, nil }},
	}
}

// check validates the plan and returns the errors of fields starting with
// prefix, so a screen reports only its own.
func (m *model) check(prefix string) error {
	err := m.plan.Validate()
	if err == nil {
		return nil
	}
	var mine []string
	for line := range strings.Lines(err.Error()) {
		if strings.HasPrefix(line, prefix) {
			mine = append(mine, strings.TrimSpace(line))
		}
	}
	if len(mine) == 0 {
		return nil
	}
	return errors.New(strings.Join(mine, "; "))
}

func same(a, b string) error {
	switch {
	case a == "":
		return errors.New("enter it twice")
	case a != b:
		return errors.New("the two entries differ")
	}
	return nil
}

func listOf(name string, values []string, current string, pick func(string)) step {
	options := make([]option, len(values))
	for i, v := range values {
		options[i] = option{label: v, value: v}
	}
	return newChoice(name, "Type to search.", options, current, pick).filtered()
}

func welcome(m *model) (step, tea.Cmd) {
	firmware := "BIOS"
	if m.env.UEFI {
		firmware = "UEFI"
	}
	hw := strings.Join(m.env.Hardware.Modules(), " ")
	if hw == "" {
		hw = "none"
	}
	body := fmt.Sprintf("This writes an install plan: a disk, a system, a user and modules.\nNothing is changed until you run it, and run asks once more.\n\nfirmware   %s\nhardware   %s\n\n%s",
		firmware, hw, faint.Render("enter to begin"))
	return &text{title: "Install Arch Linux with arcus", body: body}, nil
}

func disk(m *model) (step, tea.Cmd) {
	var options []option
	for _, d := range m.env.Disks {
		label := fmt.Sprintf("%-14s %5d GiB  %s", d.Path, d.Size>>30, d.Model)
		options = append(options, option{label: label, value: d.Path, disabled: strings.TrimPrefix(d.Problem, d.Path+" ")})
	}
	if m.plan.Disk.Device == "" {
		for _, o := range options {
			if o.disabled == "" {
				m.plan.Disk.Device = o.value
				break
			}
		}
	}
	return newChoice("Disk", "Everything on it is erased.", options, m.plan.Disk.Device, func(v string) { m.plan.Disk.Device = v }), nil
}

func profiles(m *model) (step, tea.Cmd) {
	var items []item
	for _, name := range slices.Sorted(maps.Keys(m.profiles)) {
		items = append(items, item{label: name, value: name, desc: m.profiles[name].Description, checked: slices.Contains(m.plan.Host.Profiles, name)})
	}
	return &multi{title: "Profiles", help: "Pick a profile for this machine (or none), then add modules.",
		sections: []section{{name: "profiles", items: items}},
		submit:   func(v []string) { m.plan.Host.Profiles = v }}, nil
}

// modules offers every module by category. With the installer modules,
// detected hardware starts checked, and zram and btrfs-snapshots follow the
// disk screens; with dotfiles, the chosen profiles' modules are fixed.
func modules(m *model) (step, tea.Cmd) {
	fromProfiles := map[string]bool{}
	for _, name := range m.plan.Host.Profiles {
		for _, mod := range m.profiles[name].Modules {
			fromProfiles[mod] = true
		}
	}
	dotfiles := m.plan.Dotfiles.URL != ""
	defaults := m.plan.Host.Modules
	if !dotfiles && defaults == nil {
		defaults = append([]string{"base"}, m.env.Hardware.Modules()...)
	}

	byCategory := map[string][]item{}
	for _, name := range slices.Sorted(maps.Keys(m.modules)) {
		mod := m.modules[name]
		it := item{label: name, value: name, desc: mod.Description, checked: slices.Contains(defaults, name)}
		switch {
		case name == machine.Name:
			it.checked, it.fixed, it.note = true, true, "always"
		case fromProfiles[name]:
			it.checked, it.fixed, it.note = true, true, "profile"
		case name == "btrfs-snapshots" && !dotfiles:
			it.checked, it.fixed, it.note = m.plan.Disk.Filesystem == "btrfs", true, "follows filesystem"
		}
		byCategory[mod.Category] = append(byCategory[mod.Category], it)
	}
	var sections []section
	for _, c := range slices.SortedFunc(maps.Keys(byCategory), module.CompareCategories) {
		sections = append(sections, section{name: cmp.Or(c, "other"), items: byCategory[c]})
	}
	help := "Detected hardware is checked. arcus installs these on first apply; change them later with arcus on/off."
	if dotfiles {
		help = "Your profiles' modules are fixed; add any others. arcus installs these on first apply; change them later with arcus on/off."
	}
	return &multi{title: "Modules", help: help,
		sections: sections, submit: func(v []string) {
			m.plan.Host.Modules = slices.DeleteFunc(v, func(name string) bool { return fromProfiles[name] || name == machine.Name })
		}}, nil
}

// review shows the plan as it will be saved.
type review struct {
	m               *model
	save, run, quit bool
	saved           string
	err             error
	top             int // first plan line shown
}

func (r *review) update(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	valid := r.m.plan.Validate() == nil
	switch msg.String() {
	case "up", "k":
		r.top = max(r.top-1, 0)
	case "down", "j":
		r.top = min(r.top+1, max(len(r.lines())-1, 0))
	case "s":
		r.save = valid
	case "r":
		r.run = valid
	case "q":
		r.quit = true
	}
	return false, nil
}

// lines is the plan as TOML, one entry per line.
func (r *review) lines() []string {
	var buf bytes.Buffer
	if err := r.m.plan.Encode(&buf); err != nil {
		return []string{alert.Render(err.Error())}
	}
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

func (r *review) view(height int) string {
	lines := r.lines()
	shown := max(height-8, 1)
	top := min(r.top, max(len(lines)-shown, 0))
	out := title.Render("Review") + "\n" + faint.Render("This is plan.toml. Run checks the machine again and asks you to type the disk name.") + "\n\n" +
		strings.Join(lines[top:min(top+shown, len(lines))], "\n") + "\n"
	if err := r.m.plan.Validate(); err != nil {
		out += "\n" + alert.Render(err.Error()) + "\n"
	}
	switch {
	case r.err != nil:
		out += "\n" + alert.Render("saving: "+r.err.Error()) + "\n"
	case r.saved != "":
		out += "\n" + mark.Render("saved to "+r.saved) + "\n"
	}
	return out
}
