// Package tui is the full-screen management console: providers, routing
// order, transforms, live log tail, and provider test — all against the same
// store the server uses (WAL lets both run concurrently).
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/crogers2287/cfrproxy/internal/proxy"
	"github.com/crogers2287/cfrproxy/internal/store"
)

var (
	styTab      = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("245"))
	styTabOn    = lipgloss.NewStyle().Padding(0, 2).Bold(true).Foreground(lipgloss.Color("212")).Underline(true)
	styTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	stySel      = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	styDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styOff      = lipgloss.NewStyle().Foreground(lipgloss.Color("239")).Strikethrough(true)
)

type tab int

const (
	tabProviders tab = iota
	tabTransforms
	tabLogs
	tabTest
)

type mode int

const (
	modeList mode = iota
	modeForm
	modeConfirm
)

type tickMsg time.Time

type testDoneMsg struct {
	ok      bool
	msg     string
	latency time.Duration
}

type model struct {
	s   *store.Store
	p   *proxy.Proxy
	tab tab
	mod mode

	width, height int
	status        string

	provs   []store.Provider
	trans   []store.Transform
	traces  []store.Trace
	lastTID int64
	cursor  int

	// form state
	formTitle  string
	inputs     []textinput.Model
	labels     []string
	focus      int
	editingID  int64
	formKind   string // provider | transform
	confirmMsg string
	onConfirm  func(*model)

	// test tab
	testPrompt  textinput.Model
	testRunning bool
	testResult  string
}

func Run(s *store.Store, p *proxy.Proxy) error {
	m := &model{s: s, p: p}
	m.reload()
	m.testPrompt = textinput.New()
	m.testPrompt.Placeholder = "Reply with the single word: pong"
	m.testPrompt.CharLimit = 500
	m.testPrompt.Width = 60
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m *model) reload() {
	m.provs = m.s.Providers()
	m.trans, _ = m.s.Transforms()
	ts, _ := m.s.Traces(0, 200)
	// Traces returns newest-first; keep as-is for display
	m.traces = ts
	if len(ts) > 0 {
		m.lastTID = ts[0].ID
	}
	if m.cursor >= m.listLen() {
		m.cursor = max(0, m.listLen()-1)
	}
}

func (m *model) listLen() int {
	switch m.tab {
	case tabProviders, tabTest:
		return len(m.provs)
	case tabTransforms:
		return len(m.trans)
	}
	return 0
}

func (m *model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		if m.tab == tabLogs {
			if ts, err := m.s.Traces(m.lastTID, 100); err == nil && len(ts) > 0 {
				m.traces = append(ts, m.traces...)
				if len(m.traces) > 200 {
					m.traces = m.traces[:200]
				}
				m.lastTID = m.traces[0].ID
			}
		}
		return m, tick()
	case testDoneMsg:
		m.testRunning = false
		if msg.ok {
			m.testResult = styOK.Render(fmt.Sprintf("ok (%.1fs)  ", msg.latency.Seconds())) + msg.msg
		} else {
			m.testResult = styErr.Render("failed: ") + msg.msg
		}
		return m, nil
	case tea.KeyMsg:
		switch m.mod {
		case modeForm:
			return m.updateForm(msg)
		case modeConfirm:
			switch msg.String() {
			case "y", "Y":
				m.onConfirm(m)
				m.mod = modeList
				m.reload()
			case "n", "N", "esc":
				m.mod = modeList
			}
			return m, nil
		}
		return m.updateList(msg)
	}
	return m, nil
}

func (m *model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.tab == tabTest && m.testPrompt.Focused() {
		switch msg.String() {
		case "esc":
			m.testPrompt.Blur()
		case "enter":
			m.testPrompt.Blur()
			return m, m.runTest()
		default:
			var cmd tea.Cmd
			m.testPrompt, cmd = m.testPrompt.Update(msg)
			return m, cmd
		}
		return m, nil
	}
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "1":
		m.tab, m.cursor = tabProviders, 0
	case "2":
		m.tab, m.cursor = tabTransforms, 0
	case "3":
		m.tab = tabLogs
	case "4":
		m.tab, m.cursor = tabTest, 0
	case "tab":
		m.tab = (m.tab + 1) % 4
		m.cursor = 0
	case "j", "down":
		if m.cursor < m.listLen()-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "r":
		m.reload()
		m.status = "reloaded"
	}
	switch m.tab {
	case tabProviders:
		return m.updateProviders(msg)
	case tabTransforms:
		return m.updateTransforms(msg)
	case tabTest:
		switch msg.String() {
		case "enter":
			return m, m.runTest()
		case "i":
			m.testPrompt.Focus()
			return m, textinput.Blink
		}
	}
	return m, nil
}

func (m *model) cur() *store.Provider {
	if m.cursor < len(m.provs) {
		return &m.provs[m.cursor]
	}
	return nil
}

func (m *model) updateProviders(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "a":
		m.openProviderForm(nil)
	case "e", "enter":
		if p := m.cur(); p != nil {
			m.openProviderForm(p)
		}
	case "d":
		if p := m.cur(); p != nil {
			id := p.ID
			m.confirmMsg = fmt.Sprintf("delete provider %q?", p.Name)
			m.onConfirm = func(mm *model) { mm.s.DeleteProvider(id) }
			m.mod = modeConfirm
		}
	case " ":
		if p := m.cur(); p != nil {
			cp := *p
			cp.Enabled = !cp.Enabled
			cp.APIKey = ""
			if err := m.s.SaveProvider(&cp); err != nil {
				m.status = err.Error()
			}
			m.reload()
		}
	case "J", "shift+down":
		m.moveProvider(1)
	case "K", "shift+up":
		m.moveProvider(-1)
	case "t":
		return m, m.runTest()
	}
	return m, nil
}

func (m *model) moveProvider(dir int) {
	i, j := m.cursor, m.cursor+dir
	if j < 0 || j >= len(m.provs) {
		return
	}
	ids := make([]int64, len(m.provs))
	for k, p := range m.provs {
		ids[k] = p.ID
	}
	ids[i], ids[j] = ids[j], ids[i]
	if err := m.s.Reorder(ids); err != nil {
		m.status = err.Error()
		return
	}
	m.cursor = j
	m.reload()
}

func (m *model) updateTransforms(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "a":
		m.openTransformForm(nil)
	case "e", "enter":
		if m.cursor < len(m.trans) {
			t := m.trans[m.cursor]
			m.openTransformForm(&t)
		}
	case "d":
		if m.cursor < len(m.trans) {
			id := m.trans[m.cursor].ID
			m.confirmMsg = fmt.Sprintf("delete transform %q?", m.trans[m.cursor].Name)
			m.onConfirm = func(mm *model) { mm.s.DeleteTransform(id) }
			m.mod = modeConfirm
		}
	case " ":
		if m.cursor < len(m.trans) {
			t := m.trans[m.cursor]
			m.s.SetTransformEnabled(t.ID, !t.Enabled)
			m.reload()
		}
	}
	return m, nil
}

func (m *model) runTest() tea.Cmd {
	p := m.cur()
	if p == nil || m.testRunning {
		return nil
	}
	prompt := m.testPrompt.Value()
	if prompt == "" {
		prompt = "Reply with the single word: pong"
	}
	prov := *p
	m.testRunning = true
	m.testResult = "testing " + prov.Name + "…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		start := time.Now()
		resp, err := m.p.TestProvider(ctx, prov, prompt)
		if err != nil {
			return testDoneMsg{ok: false, msg: err.Error(), latency: time.Since(start)}
		}
		return testDoneMsg{ok: true, msg: resp.Content, latency: time.Since(start)}
	}
}

// ---- forms ----

func mkInput(label, val string, width int) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = label
	ti.SetValue(val)
	ti.CharLimit = 4096
	ti.Width = width
	return ti
}

func (m *model) openProviderForm(p *store.Provider) {
	m.formKind = "provider"
	m.labels = []string{"name", "type (openai|anthropic|ollama)", "base_url", "api_key (blank = keep)", "default_model", "models (aliases, comma-sep)", "fallback (provider/model)", "doc_url", "inject_docs (y/n)"}
	var vals [9]string
	m.editingID = 0
	m.formTitle = "add provider"
	if p != nil {
		m.editingID = p.ID
		m.formTitle = "edit " + p.Name
		inject := "n"
		if p.InjectDocs {
			inject = "y"
		}
		vals = [9]string{p.Name, p.Type, p.BaseURL, "", p.DefaultModel, p.Models, p.Fallback, p.DocURL, inject}
	} else {
		vals[8] = "n"
	}
	m.inputs = nil
	for i, l := range m.labels {
		m.inputs = append(m.inputs, mkInput(l, vals[i], 60))
	}
	m.focus = 0
	m.inputs[0].Focus()
	m.mod = modeForm
}

func (m *model) openTransformForm(t *store.Transform) {
	m.formKind = "transform"
	m.labels = []string{"name", "phase (request|response)", "target dialect (blank = any)", "provider name (blank = all)", "rules JSON"}
	var vals [5]string
	m.editingID = 0
	m.formTitle = "add transform"
	if t != nil {
		m.editingID = t.ID
		m.formTitle = "edit " + t.Name
		provName := ""
		if t.ProviderID != 0 {
			if p, ok := m.s.ProviderByID(t.ProviderID); ok {
				provName = p.Name
			}
		}
		vals = [5]string{t.Name, t.Phase, t.Target, provName, string(t.Rules)}
	} else {
		vals[1] = "request"
		vals[4] = `[{"op":"set","path":"temperature","value":0.2}]`
	}
	m.inputs = nil
	for i, l := range m.labels {
		m.inputs = append(m.inputs, mkInput(l, vals[i], 80))
	}
	m.focus = 0
	m.inputs[0].Focus()
	m.mod = modeForm
}

func (m *model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mod = modeList
		return m, nil
	case "tab", "enter", "down":
		if msg.String() == "enter" && m.focus == len(m.inputs)-1 {
			if err := m.submitForm(); err != nil {
				m.status = err.Error()
				return m, nil
			}
			m.mod = modeList
			m.reload()
			m.status = "saved"
			return m, nil
		}
		m.inputs[m.focus].Blur()
		m.focus = (m.focus + 1) % len(m.inputs)
		m.inputs[m.focus].Focus()
		return m, textinput.Blink
	case "shift+tab", "up":
		m.inputs[m.focus].Blur()
		m.focus = (m.focus - 1 + len(m.inputs)) % len(m.inputs)
		m.inputs[m.focus].Focus()
		return m, textinput.Blink
	}
	var cmd tea.Cmd
	m.inputs[m.focus], cmd = m.inputs[m.focus].Update(msg)
	return m, cmd
}

func (m *model) submitForm() error {
	v := func(i int) string { return strings.TrimSpace(m.inputs[i].Value()) }
	if m.formKind == "provider" {
		p := store.Provider{ID: m.editingID, Enabled: true}
		if m.editingID != 0 {
			if exist, ok := m.s.ProviderByID(m.editingID); ok {
				p = exist
				p.APIKey = ""
			}
		}
		p.Name, p.Type = v(0), v(1)
		if v(3) != "" {
			p.APIKey = v(3)
		}
		p.DefaultModel, p.Models, p.Fallback, p.DocURL = v(4), v(5), v(6), v(7)
		p.InjectDocs = strings.HasPrefix(strings.ToLower(v(8)), "y")
		if v(2) != p.BaseURL {
			p.BaseURL = v(2)
			probe := p
			if probe.APIKey == "" && m.editingID != 0 {
				if exist, ok := m.s.ProviderByID(m.editingID); ok {
					probe.APIKey = exist.APIKey
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			base, note := m.p.DiscoverBase(ctx, probe)
			cancel()
			p.BaseURL = base
			if note != "" {
				m.status = note
			}
		}
		return m.s.SaveProvider(&p)
	}
	t := store.Transform{ID: m.editingID, Enabled: true, Name: v(0), Phase: v(1), Target: v(2), Rules: json.RawMessage(v(4))}
	if m.editingID != 0 {
		for _, ex := range m.trans {
			if ex.ID == m.editingID {
				t.Enabled = ex.Enabled
			}
		}
	}
	if v(3) != "" {
		p, ok := m.s.ProviderByName(v(3))
		if !ok {
			return fmt.Errorf("provider %q not found", v(3))
		}
		t.ProviderID = p.ID
	}
	return m.s.SaveTransform(&t)
}

// ---- view ----

func (m *model) View() string {
	var b strings.Builder
	tabs := []string{"1 Providers", "2 Transforms", "3 Logs", "4 Test"}
	var row []string
	for i, t := range tabs {
		if tab(i) == m.tab {
			row = append(row, styTabOn.Render(t))
		} else {
			row = append(row, styTab.Render(t))
		}
	}
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, row...) + "\n\n")

	switch m.mod {
	case modeForm:
		b.WriteString(styTitle.Render(m.formTitle) + "\n\n")
		for i := range m.inputs {
			cursor := "  "
			if i == m.focus {
				cursor = stySel.Render("> ")
			}
			b.WriteString(fmt.Sprintf("%s%-32s %s\n", cursor, styDim.Render(m.labels[i]), m.inputs[i].View()))
		}
		b.WriteString("\n" + styHelp.Render("enter on last field = save · tab/↑↓ = move · esc = cancel"))
	case modeConfirm:
		b.WriteString(styErr.Render(m.confirmMsg) + styHelp.Render("  [y/n]"))
	default:
		switch m.tab {
		case tabProviders:
			m.viewProviders(&b)
		case tabTransforms:
			m.viewTransforms(&b)
		case tabLogs:
			m.viewLogs(&b)
		case tabTest:
			m.viewTest(&b)
		}
	}
	if m.status != "" {
		b.WriteString("\n\n" + styDim.Render(m.status))
	}
	return b.String()
}

func (m *model) viewProviders(b *strings.Builder) {
	if len(m.provs) == 0 {
		b.WriteString(styDim.Render("no providers yet — press a to add one") + "\n")
	}
	for i, p := range m.provs {
		cursor := "  "
		if i == m.cursor {
			cursor = stySel.Render("> ")
		}
		key := styDim.Render("key:-")
		if p.HasKey {
			key = styOK.Render("key:✓")
		}
		line := fmt.Sprintf("%-16s %-10s %-38s %s model:%s", p.Name, p.Type, p.BaseURL, key, p.DefaultModel)
		if !p.Enabled {
			line = styOff.Render(line)
		} else if i == m.cursor {
			line = stySel.Render(line)
		}
		b.WriteString(cursor + line + "\n")
	}
	b.WriteString("\n" + styHelp.Render("a add · e edit · d delete · space en/disable · J/K reorder · t test · 1-4 tabs · q quit"))
	if m.testResult != "" {
		b.WriteString("\n\n" + m.testResult)
	}
}

func (m *model) viewTransforms(b *strings.Builder) {
	if len(m.trans) == 0 {
		b.WriteString(styDim.Render("no transforms yet — press a to add one") + "\n")
	}
	for i, t := range m.trans {
		cursor := "  "
		if i == m.cursor {
			cursor = stySel.Render("> ")
		}
		scope := "all"
		if t.ProviderID != 0 {
			if p, ok := m.s.ProviderByID(t.ProviderID); ok {
				scope = p.Name
			}
		}
		tgt := t.Target
		if tgt == "" {
			tgt = "any"
		}
		rules := string(t.Rules)
		if len(rules) > 60 {
			rules = rules[:60] + "…"
		}
		line := fmt.Sprintf("%-20s %-8s scope:%-12s target:%-9s %s", t.Name, t.Phase, scope, tgt, rules)
		if !t.Enabled {
			line = styOff.Render(line)
		} else if i == m.cursor {
			line = stySel.Render(line)
		}
		b.WriteString(cursor + line + "\n")
	}
	b.WriteString("\n" + styHelp.Render("a add · e edit · d delete · space en/disable · 1-4 tabs · q quit"))
}

func (m *model) viewLogs(b *strings.Builder) {
	b.WriteString(styTitle.Render("live request traces") + styDim.Render("  (auto-refreshing)") + "\n\n")
	maxRows := max(5, m.height-8)
	rows := m.traces
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}
	if len(rows) == 0 {
		b.WriteString(styDim.Render("no traffic yet") + "\n")
	}
	for _, t := range rows {
		status := styOK.Render(fmt.Sprintf("%3d", t.Status))
		if t.Status >= 400 || t.Err != "" {
			status = styErr.Render(fmt.Sprintf("%3d", t.Status))
		}
		stream := "     "
		if t.Stream {
			stream = "strm "
		}
		line := fmt.Sprintf("%s  %-12s %-24s %-9s %s %s%5dms", time.UnixMilli(t.TS).Format("15:04:05"),
			t.Provider, trunc(t.Model, 24), t.Inbound, status, stream, t.LatencyMS)
		if t.Err != "" {
			line += "  " + styErr.Render(trunc(t.Err, 60))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + styHelp.Render("1-4 tabs · q quit"))
}

func (m *model) viewTest(b *strings.Builder) {
	b.WriteString(styTitle.Render("test a provider") + "\n\n")
	for i, p := range m.provs {
		cursor := "  "
		line := p.Name + "  " + styDim.Render(p.DefaultModel)
		if i == m.cursor {
			cursor = stySel.Render("> ")
			line = stySel.Render(p.Name) + "  " + styDim.Render(p.DefaultModel)
		}
		b.WriteString(cursor + line + "\n")
	}
	b.WriteString("\nprompt: " + m.testPrompt.View() + "\n")
	if m.testRunning {
		b.WriteString("\n" + styDim.Render("running…"))
	} else if m.testResult != "" {
		b.WriteString("\n" + m.testResult)
	}
	b.WriteString("\n\n" + styHelp.Render("i edit prompt · enter run · j/k select · 1-4 tabs · q quit"))
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
