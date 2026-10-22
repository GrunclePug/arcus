package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The teal and gold palette.
var (
	teal  = lipgloss.Color("#5fd4c4")
	gold  = lipgloss.Color("#e5c07b")
	red   = lipgloss.Color("#ef6b73")
	title = lipgloss.NewStyle().Bold(true).Foreground(teal)
	faint = lipgloss.NewStyle().Faint(true)
	focus = lipgloss.NewStyle().Foreground(teal)
	mark  = lipgloss.NewStyle().Foreground(gold)
	alert = lipgloss.NewStyle().Foreground(red)
)

// step is one screen. update gets the keys the parent doesn't handle and
// reports when the screen is done; view renders it in height lines.
type step interface {
	update(msg tea.KeyPressMsg) (done bool, cmd tea.Cmd)
	view(height int) string
}

// window returns the range of n lines to show so that line cursor is
// visible in height lines.
func window(n, cursor, height int) (int, int) {
	if height < 1 || n <= height {
		return 0, n
	}
	start := min(max(cursor-height/2, 0), n-height)
	return start, start + height
}

// option is one entry of a choice; a disabled one says why.
type option struct {
	label, value, disabled string
}

// choice picks one option. With filter set, typing narrows the list.
type choice struct {
	title, help string
	options     []option
	cursor      int
	filter      *textinput.Model
	pick        func(value string)
}

func newChoice(title, help string, options []option, current string, pick func(string)) *choice {
	c := &choice{title: title, help: help, options: options, pick: pick}
	for i, o := range options {
		if o.value == current {
			c.cursor = i
		}
	}
	return c
}

// filtered makes the choice searchable, for long lists.
func (c *choice) filtered() *choice {
	in := textinput.New()
	in.Prompt = "search: "
	in.Focus()
	c.filter = &in
	return c
}

func (c *choice) visible() []option {
	if c.filter == nil || c.filter.Value() == "" {
		return c.options
	}
	var out []option
	for _, o := range c.options {
		if strings.Contains(strings.ToLower(o.label), strings.ToLower(c.filter.Value())) {
			out = append(out, o)
		}
	}
	return out
}

func (c *choice) update(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	shown := c.visible()
	switch msg.String() {
	case "up", "ctrl+p":
		c.cursor = max(c.cursor-1, 0)
	case "down", "ctrl+n":
		c.cursor = min(c.cursor+1, len(shown)-1)
	case "enter":
		if c.cursor < len(shown) && shown[c.cursor].disabled == "" {
			c.pick(shown[c.cursor].value)
			return true, nil
		}
	default:
		if c.filter != nil {
			before := c.filter.Value()
			in, cmd := c.filter.Update(msg)
			c.filter = &in
			if in.Value() != before {
				c.cursor = 0
			}
			return false, cmd
		}
		switch msg.String() {
		case "k":
			c.cursor = max(c.cursor-1, 0)
		case "j":
			c.cursor = min(c.cursor+1, len(shown)-1)
		}
	}
	return false, nil
}

func (c *choice) view(height int) string {
	var b strings.Builder
	b.WriteString(title.Render(c.title) + "\n")
	if c.help != "" {
		b.WriteString(faint.Render(c.help) + "\n")
	}
	if c.filter != nil {
		b.WriteString(c.filter.View() + "\n")
	}
	b.WriteString("\n")
	shown := c.visible()
	start, end := window(len(shown), c.cursor, height-6)
	for i := start; i < end; i++ {
		o := shown[i]
		line := "  " + o.label
		switch {
		case o.disabled != "":
			line = faint.Render(fmt.Sprintf("  %s  (%s)", o.label, o.disabled))
		case i == c.cursor:
			line = focus.Render("> " + o.label)
		}
		b.WriteString(line + "\n")
	}
	if len(shown) == 0 {
		b.WriteString(faint.Render("  no match") + "\n")
	}
	return b.String()
}

// field is one text input of a form.
type field struct {
	label string
	input textinput.Model
}

func newField(label, value string, secret bool) field {
	in := textinput.New()
	in.SetValue(value)
	if secret {
		in.EchoMode = textinput.EchoPassword
	}
	return field{label: label, input: in}
}

// form edits text fields; submit checks them and stores their values.
// Enter moves to the next field and submits on the last one, or on any
// field once enough says the rest doesn't matter.
type form struct {
	title, help string
	fields      []field
	focus       int
	err         string
	submit      func(values []string) error
	enough      func(values []string) bool
}

func newForm(title, help string, submit func([]string) error, fields ...field) *form {
	f := &form{title: title, help: help, fields: fields, submit: submit}
	f.fields[0].input.Focus()
	return f
}

func (f *form) move(to int) tea.Cmd {
	f.fields[f.focus].input.Blur()
	f.focus = (to + len(f.fields)) % len(f.fields)
	return f.fields[f.focus].input.Focus()
}

func (f *form) update(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "tab", "down":
		return false, f.move(f.focus + 1)
	case "shift+tab", "up":
		return false, f.move(f.focus - 1)
	case "enter":
		values := make([]string, len(f.fields))
		for i, fl := range f.fields {
			values[i] = strings.TrimSpace(fl.input.Value())
		}
		if f.focus < len(f.fields)-1 && (f.enough == nil || !f.enough(values)) {
			return false, f.move(f.focus + 1)
		}
		if err := f.submit(values); err != nil {
			f.err = err.Error()
			return false, nil
		}
		return true, nil
	}
	var cmd tea.Cmd
	f.fields[f.focus].input, cmd = f.fields[f.focus].input.Update(msg)
	f.err = ""
	return false, cmd
}

func (f *form) view(int) string {
	var b strings.Builder
	b.WriteString(title.Render(f.title) + "\n")
	if f.help != "" {
		b.WriteString(faint.Render(f.help) + "\n")
	}
	b.WriteString("\n")
	for i, fl := range f.fields {
		label := fmt.Sprintf("%-22s", fl.label)
		if i == f.focus {
			label = focus.Render(label)
		}
		b.WriteString(label + fl.input.View() + "\n")
	}
	if f.err != "" {
		b.WriteString("\n" + alert.Render(f.err) + "\n")
	}
	return b.String()
}

// item is one entry of a multi-select. A fixed one follows another choice
// and can't be toggled; note says which.
type item struct {
	label, value, desc, note string
	checked, fixed           bool
}

// section groups items under a heading, such as a module category.
type section struct {
	name  string
	items []item
}

// multi selects any number of items, section by section.
type multi struct {
	title, help string
	sections    []section
	cursor      int
	submit      func(values []string)
}

func (m *multi) at(i int) *item {
	for s := range m.sections {
		if i < len(m.sections[s].items) {
			return &m.sections[s].items[i]
		}
		i -= len(m.sections[s].items)
	}
	return nil
}

func (m *multi) count() int {
	n := 0
	for _, s := range m.sections {
		n += len(s.items)
	}
	return n
}

func (m *multi) update(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "down", "j":
		m.cursor = min(m.cursor+1, m.count()-1)
	case "space", "x":
		if it := m.at(m.cursor); it != nil && !it.fixed {
			it.checked = !it.checked
		}
	case "enter":
		var values []string
		for _, s := range m.sections {
			for _, it := range s.items {
				if it.checked {
					values = append(values, it.value)
				}
			}
		}
		m.submit(values)
		return true, nil
	}
	return false, nil
}

func (m *multi) view(height int) string {
	var lines []string
	cursorLine, n := 0, 0
	for _, s := range m.sections {
		lines = append(lines, mark.Render(s.name))
		for _, it := range s.items {
			box := "[ ]"
			if it.checked {
				box = "[x]"
			}
			line := fmt.Sprintf("  %s %-18s", box, it.label)
			if n == m.cursor {
				line = focus.Render(fmt.Sprintf("> %s %-18s", box, it.label))
				cursorLine = len(lines)
			}
			line += " " + faint.Render(it.desc)
			if it.note != "" {
				line += " " + faint.Render("("+it.note+")")
			}
			lines = append(lines, line)
			n++
		}
	}
	var b strings.Builder
	b.WriteString(title.Render(m.title) + "\n")
	if m.help != "" {
		b.WriteString(faint.Render(m.help) + "\n")
	}
	b.WriteString("\n")
	start, end := window(len(lines), cursorLine, height-4)
	b.WriteString(strings.Join(lines[start:end], "\n") + "\n")
	return b.String()
}

// text is a screen to read; enter goes on.
type text struct {
	title, body string
}

func (t *text) update(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	return msg.String() == "enter", nil
}

func (t *text) view(int) string {
	return title.Render(t.title) + "\n\n" + t.body + "\n"
}
