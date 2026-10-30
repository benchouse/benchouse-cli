package tui

import (
	"fmt"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/prompt"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const privacyURL = "https://benchouse.ai/privacy"

// View implements tea.Model: header, body, status bar. The whole screen is
// redrawn on every change, so moving to the next step starts on a clean
// screen.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	header := m.headerView()
	status := m.statusView()
	h := m.bodyHeight()
	body := lipgloss.NewStyle().Width(m.width).Height(h).MaxHeight(h).Padding(1, 2, 0, 2).Render(m.bodyView())
	return lipgloss.JoinVertical(lipgloss.Left, header, body, status)
}

func (m *Model) bodyWidth() int { return max(m.width-4, 20) }

// bodyHeight is what is left between the header (2 lines) and the status bar.
func (m *Model) bodyHeight() int { return max(m.height-2-len(m.statusLines()), 3) }

// viewportHeight leaves room for a title and the hint lines around a
// scrolling text.
func (m *Model) viewportHeight() int { return max(m.bodyHeight()-6, 3) }

// --- header -------------------------------------------------------------

// total is the number of steps: agent, conversations, dbt, [context,] review.
func (m *Model) total() int {
	if m.other > 0 {
		return 2 // the request-an-agent flow: name, email
	}
	a := m.agent
	if a == nil && m.cursor < len(m.cfg.Agents) {
		a = &m.cfg.Agents[m.cursor]
	}
	if a != nil && a.Context != nil {
		return 5
	}
	return 4
}

func (m *Model) stepNumber() int {
	if m.other > 0 {
		return m.other
	}
	switch m.step {
	case stepAgent:
		return 1
	case stepConv:
		return 2
	case stepDBT:
		return 3
	case stepCtx:
		return 4
	}
	return m.total()
}

func (m *Model) stepName() string {
	switch m.step {
	case stepAgent:
		if m.other > 0 {
			return "Request an agent"
		}
		return "Agent"
	case stepConv:
		return prompt.Clean(m.agent.Name) + " conversations"
	case stepDBT:
		return "dbt project"
	case stepCtx:
		return prompt.Clean(m.agent.Context.Label)
	}
	if m.cfg.Upload {
		return "Review & upload"
	}
	return "Review & save"
}

func (m *Model) headerView() string {
	sep := grayStyle.Render("  ·  ")
	line := " " + boldStyle.Render("benchouse") + sep +
		accentBold.Render(fmt.Sprintf("Step %d/%d", m.stepNumber(), m.total())) + sep + m.stepName()
	if m.canGoBack() {
		line += sep + grayStyle.Render("← esc")
	}
	return lipgloss.NewStyle().MaxWidth(m.width).Render(line) + "\n" + ruleStyle.Render(strings.Repeat("─", max(m.width, 1)))
}

// canGoBack reports whether esc leads to an earlier screen from here: on
// every step after the first, except while busy (esc cancels) and once
// the run is over.
func (m *Model) canGoBack() bool {
	if m.other == 1 && m.phase == phForm {
		return true // back to the agent list
	}
	if m.stepNumber() < 2 {
		return false
	}
	switch m.phase {
	case phBusy, phSent, phReceipt:
		return false
	case phSetup:
		return m.cfg.Agent == nil
	}
	return true
}

// --- status bar ---------------------------------------------------------

func glyph(v verdict) string {
	switch v {
	case vOK:
		return "✓"
	case vFailed:
		return accentStyle.Render("✗")
	case vSkipped:
		return "skipped"
	case vUnverified:
		return "–"
	}
	return "…"
}

// statusText is the state of the run: connection · dbt · context · people.
// The header names the agent. Empty before an agent is chosen and in the
// request-an-agent flow.
func (m *Model) statusText() string {
	sep := grayStyle.Render(" · ")
	if m.agent == nil || m.step == stepAgent {
		return ""
	}
	conn := glyph(m.conn.v)
	if m.op == opCheck && m.step == stepConv || m.op == opLookup || m.op == opVerify {
		conn = m.spin.View()
	}
	parts := []string{"connection " + conn, "dbt " + glyph(m.dbt.v)}
	if m.agent.Context != nil {
		parts = append(parts, "context "+glyph(m.ctxc.v))
	}
	people := "pseudonymised"
	if !m.anonymize {
		people = "real"
	}
	parts = append(parts, "people "+people)
	return strings.Join(parts, sep)
}

// hints are the keys that do something on this screen.
func (m *Model) hints() string {
	var h []string
	switch m.phase {
	case phList:
		h = []string{"↑/↓ choose", "enter next"}
	case phSetup:
		h = []string{"↑/↓ scroll", "v queries", "enter next"}
		if m.cfg.Agent == nil {
			h = append(h, "esc back")
		}
	case phForm:
		h = []string{"enter next", "shift+tab previous field", "esc back"}
	case phBusy:
		h = []string{"esc cancel"}
	case phChecked:
		if m.state(m.step).v == vFailed {
			h = []string{"enter edit", "r retry"}
		} else {
			h = []string{"enter next", "e edit"}
			if m.step == stepConv {
				h = append(h, "v queries")
			}
		}
		if m.step == stepDBT || m.step == stepCtx {
			h = append(h, "s skip")
		}
		h = append(h, "esc back")
	case phQueries:
		h = []string{"↑/↓ scroll", "esc back"}
	case phSent, phReceipt:
		h = []string{"enter done"}
	case phRequestErr:
		h = []string{"r retry", "enter edit", "esc back"}
	case phSummary:
		h = []string{"↑/↓ choose", "enter select", "esc back"}
	case phCollectErr:
		h = []string{"r retry", "enter back", "esc back"}
	case phUploadErr:
		h = []string{"r retry", "enter finish without uploading", "esc back"}
	}
	h = append(h, "ctrl+c quit")
	return grayStyle.Render(strings.Join(h, " · "))
}

// statusLines is the status bar: a rule, then the keys on the left and the
// state of the run on the right. When they do not fit, the keys win: the
// state is cut from its left end, and the keys are clipped only when they
// alone are too wide.
func (m *Model) statusLines() []string {
	keys, st := " "+m.hints(), m.statusText()
	lines := []string{ruleStyle.Render(strings.Repeat("─", max(m.width, 1)))}
	kw := lipgloss.Width(keys)
	if kw >= m.width || st == "" {
		return append(lines, ansi.Truncate(keys, m.width, ""))
	}
	room := m.width - kw - 3 // two spaces between, one at the right edge
	if sw := lipgloss.Width(st); sw > room {
		if room < 2 {
			return append(lines, keys)
		}
		st = "…" + ansi.TruncateLeft(st, sw-room+1, "")
	}
	gap := m.width - kw - lipgloss.Width(st) - 1
	return append(lines, keys+strings.Repeat(" ", gap)+st+" ")
}

func (m *Model) statusView() string { return strings.Join(m.statusLines(), "\n") }

// --- body ---------------------------------------------------------------

func (m *Model) bodyView() string {
	w := m.bodyWidth()
	wrap := lipgloss.NewStyle().Width(w)
	switch m.phase {
	case phList:
		return m.listView()
	case phSetup:
		var b strings.Builder
		b.WriteString(boldStyle.Render("Where "+prompt.Clean(m.agent.Name)+" keeps its conversations") + "\n")
		if m.agent.Status != agent.Supported {
			b.WriteString(wrap.Inherit(grayStyle).Render(fmt.Sprintf("%s: written from %s's published schema and not yet run against a live deployment; tell us if anything looks off.",
				m.agent.Status, prompt.Clean(m.agent.Name))) + "\n")
		}
		b.WriteString("\n" + m.viewport.View() + "\n\n")
		if !m.viewport.AtBottom() {
			b.WriteString(grayStyle.Render(fmt.Sprintf("%d%% · ↓ for more", int(m.viewport.ScrollPercent()*100))) + "\n")
		}
		b.WriteString(grayStyle.Render(fmt.Sprintf("v view the %d read-only queries before anything connects", m.queryCount())))
		return b.String()
	case phForm:
		return m.formView()
	case phBusy:
		s := m.spin.View() + " " + m.busyLabel
		if m.op == opCollect && len(m.notes) > 0 {
			keep := max(m.bodyHeight()-4, 1)
			notes := m.notes
			if len(notes) > keep {
				notes = notes[len(notes)-keep:]
			}
			for _, n := range notes {
				s += "\n" + wrap.Inherit(grayStyle).Render(prompt.Clean(n))
			}
		}
		return s
	case phChecked:
		return m.checkedView()
	case phQueries:
		return boldStyle.Render(fmt.Sprintf("%d read-only queries", m.queryCount())) + "\n" +
			grayStyle.Render("Results stay on this machine until you approve the upload.") + "\n\n" + m.viewport.View()
	case phSent:
		s := accentStyle.Render("✓ ") + wrap.Render(fmt.Sprintf("Thanks. We've noted %s.", prompt.Clean(m.outcome.RequestedAgent)))
		if m.outcome.Email != "" {
			s += "\n  " + wrap.Render(fmt.Sprintf("We'll email %s when it's supported, typically within 72 hours.", prompt.Clean(m.outcome.Email)))
		} else {
			s += "\n  " + wrap.Inherit(grayStyle).Render("No email — that's fine, we've noted the agent.")
		}
		return s
	case phRequestErr:
		return errLine(w, m.requestErr)
	case phSummary:
		return m.summaryView()
	case phReceipt:
		return "✓ " + wrap.Render(fmt.Sprintf("Uploaded (%s, reference %s).", human(m.receipt.Bytes), boldStyle.Render(prompt.Clean(m.receipt.ID)))) + "\n" +
			wrap.Render(fmt.Sprintf("André will email %s the top issues within a few days, and can walk you through the full findings in 30 minutes if you'd like.", prompt.Clean(m.normEmail(m.email)))) + "\n\n" +
			wrap.Inherit(grayStyle).Render("The bundle stays at "+prompt.Clean(m.bundle.Path))
	case phCollectErr:
		return errLine(w, m.collectErr)
	case phUploadErr:
		return errLine(w, m.uploadErr) + "\n\n" + wrap.Render("Send it later with:") + "\n  " +
			boldStyle.Render(prompt.Clean(fmt.Sprintf("benchouse send %s --email %s", m.bundle.Rel, m.normEmail(m.email))))
	}
	return ""
}

func errLine(w int, err error) string {
	if err == nil {
		return ""
	}
	return accentStyle.Render("✗ ") + lipgloss.NewStyle().Width(w-2).Render(prompt.Clean(err.Error()))
}

func (m *Model) listView() string {
	w := m.bodyWidth()
	var b strings.Builder
	b.WriteString(boldStyle.Render("Find out where your analytics agent gets answers wrong") + "\n")
	b.WriteString(lipgloss.NewStyle().Width(w).Foreground(gray).Render("This reads your agent's conversations on this machine, shows you exactly what it collected, and uploads it only when you say so. Credentials never leave this machine.") + "\n\n")
	b.WriteString(boldStyle.Render("Which agent do you use?") + "\n")
	type item struct{ label, note string }
	var items []item
	for _, a := range m.cfg.Agents {
		it := item{label: prompt.Clean(a.Name)}
		items = append(items, it)
	}
	items = append(items, item{label: "Other"})
	// Scroll the list when the terminal is short.
	room := max(m.bodyHeight()-lipgloss.Height(b.String())-3, 3)
	first := 0
	if len(items) > room {
		first = min(max(m.cursor-room/2, 0), len(items)-room)
	}
	for i := first; i < len(items) && i < first+room; i++ {
		it := items[i]
		line := "  " + it.label
		if i == m.cursor {
			line = accentStyle.Render("❯ " + it.label)
		}
		if it.note != "" {
			line += "  " + grayStyle.Render(it.note)
		}
		b.WriteString(line + "\n")
	}
	if m.cursor < len(m.cfg.Agents) {
		b.WriteString("\n" + lipgloss.NewStyle().Width(w).Foreground(gray).Render(prompt.Clean(m.cfg.Agents[m.cursor].Summary)))
	} else {
		b.WriteString("\n" + grayStyle.Render("We'll ask for its name and your email."))
	}
	return b.String()
}

func (m *Model) formView() string {
	w := m.bodyWidth()
	var b strings.Builder
	switch m.fk {
	case fkOther:
		b.WriteString(boldStyle.Render("Tell us which agent you use") + "\n")
		b.WriteString(grayStyle.Render("We add agents people ask for, typically within 72 hours.") + "\n\n")
	case fkOtherEmail:
		b.WriteString(boldStyle.Render("Hear when "+prompt.Clean(strings.TrimSpace(m.otherName))+" is supported") + grayStyle.Render(" (optional)") + "\n\n")
	case fkConv:
		b.WriteString(boldStyle.Render("Connect to "+prompt.Clean(m.agent.Name)+"'s conversations") + "\n")
		b.WriteString(grayStyle.Render("Credentials are used on this machine only and never written to the bundle.") + "\n\n")
		if m.lookupMsg != "" {
			b.WriteString(lipgloss.NewStyle().Width(w).Foreground(gray).Render(m.lookupMsg) + "\n\n")
		}
	case fkDBT:
		b.WriteString(boldStyle.Render("dbt project") + grayStyle.Render(" (optional)") + "\n\n")
	case fkCtx:
		b.WriteString(boldStyle.Render(prompt.Clean(m.agent.Context.Label)) + grayStyle.Render(" (optional)") + "\n\n")
	case fkReview:
		b.WriteString(boldStyle.Render("Before collecting") + "\n")
		b.WriteString(lipgloss.NewStyle().Width(w).Render(m.windowText()) + "\n\n")
	}
	if m.form != nil {
		b.WriteString(m.form.View())
	}
	if m.fk == fkConv {
		b.WriteString(m.verifiedView(w))
	}
	return b.String()
}

// verifiedView is what the connection forms' live checks found so far, in
// field order, and the error of the one that failed.
func (m *Model) verifiedView(w int) string {
	var b strings.Builder
	wrap := lipgloss.NewStyle().Width(w - 2)
	for _, f := range m.agent.Fields {
		if fact := m.verified[f.Key]; fact != "" {
			b.WriteString("✓ " + wrap.Render(prompt.Clean(fact)) + "\n")
		}
	}
	if m.verifyErr != nil {
		b.WriteString(errLine(w, m.verifyErr) + "\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\n" + b.String()
}

// windowText says how conversations are chosen, with the connection
// check's count when there is one.
func (m *Model) windowText() string {
	s := "The most recent conversations in the window are collected, up to the cap: whichever limit is hit first."
	if n := m.conn.res.Conversations; n != nil {
		s += fmt.Sprintf(" The connection check counted %s %s.", agent.Thousands(*n), agent.Window(m.cfg.Opts))
	}
	return s
}

func (m *Model) checkedView() string {
	w := m.bodyWidth()
	wrap := lipgloss.NewStyle().Width(w - 2)
	st := m.state(m.step)
	var b strings.Builder
	switch m.step {
	case stepConv:
		b.WriteString(boldStyle.Render("Connection to "+prompt.Clean(m.agent.Name)) + "\n\n")
		// An entry picked automatically (the only nao project) is only worth
		// a line when no check will report what it connected to.
		if m.agent.Check == nil {
			for _, p := range m.picked {
				b.WriteString("✓ " + wrap.Render(p) + "\n")
			}
		}
	case stepDBT:
		b.WriteString(boldStyle.Render("dbt project") + "\n")
		b.WriteString(grayStyle.Render(prompt.Clean(st.dir)) + "\n\n")
	case stepCtx:
		b.WriteString(boldStyle.Render(prompt.Clean(m.agent.Context.Label)) + "\n")
		b.WriteString(grayStyle.Render(prompt.Clean(st.dir)) + "\n\n")
	}
	switch st.v {
	case vFailed:
		b.WriteString(errLine(w, st.err) + "\n\n")
		b.WriteString(grayStyle.Render("enter to correct it (your answers are kept), r to try again") + "\n")
		return b.String()
	case vUnverified:
		b.WriteString(grayStyle.Render("– will be verified while collecting") + "\n")
	case vOK:
		for _, f := range st.res.Facts {
			b.WriteString("✓ " + wrap.Render(prompt.Clean(f)) + "\n")
		}
		if len(st.res.Facts) == 0 {
			b.WriteString("✓ looks right\n")
		}
		for _, x := range st.res.Warnings {
			b.WriteString(accentStyle.Render("! ") + wrap.Render(prompt.Clean(x)) + "\n")
		}
	}
	if m.step == stepConv {
		n := m.queryCount()
		b.WriteString("\n" + lipgloss.NewStyle().Width(w).Render(fmt.Sprintf("The CLI will run %d read-only queries. Results stay on this machine until you approve the upload.", n)) + "\n")
		if !m.results {
			b.WriteString(lipgloss.NewStyle().Width(w).Foreground(gray).Render("Rows your agent's queries returned are left out by default; only columns and row counts are kept.") + "\n")
		}
		b.WriteString(grayStyle.Render("What we keep and why: "+privacyURL) + "\n\n")
		b.WriteString(grayStyle.Render(fmt.Sprintf("v view the %d read-only queries", n)))
	}
	return b.String()
}

func (m *Model) summaryView() string {
	w := m.bodyWidth()
	bnd := m.bundle
	var top strings.Builder
	top.WriteString(boldStyle.Render("What's in the bundle") + "\n")
	var acts strings.Builder
	acts.WriteString(lipgloss.NewStyle().Width(w).Foreground(gray).Render("Bundle: "+prompt.Clean(bnd.Path)) + "\n")
	if m.cfg.Upload {
		acts.WriteString(lipgloss.NewStyle().Width(w).Render(fmt.Sprintf("Upload %s to Benchouse? Findings go to %s.", human(bnd.Size), prompt.Clean(m.normEmail(m.email)))) + "\n")
	}
	for i, a := range m.actions() {
		if i == m.menu {
			acts.WriteString(accentStyle.Render("❯ "+a) + "\n")
		} else {
			acts.WriteString("  " + a + "\n")
		}
	}
	// The summary box gets whatever height is left; a long one is cut with
	// a pointer to the full listing.
	room := m.bodyHeight() - 1 - lipgloss.Height(top.String()) - lipgloss.Height(acts.String()) - 2
	boxW := min(w, 100) - 2 // lipgloss widths include padding, not the border
	raw := strings.Split(strings.TrimRight(prompt.Clean(bnd.Summary), "\n"), "\n")
	for i, l := range raw {
		raw[i] = strings.TrimPrefix(l, "  ")
	}
	wrapped := lipgloss.NewStyle().Width(boxW - 2).Render(strings.Join(raw, "\n"))
	lines := strings.Split(wrapped, "\n")
	if room < len(lines) && room >= 2 {
		cut := len(lines) - room + 1
		lines = append(lines[:room-1], grayStyle.Render(fmt.Sprintf("… %d more lines: benchouse inspect %s", cut, prompt.Clean(bnd.Rel))))
	}
	box := boxStyle.Width(boxW).Render(strings.Join(lines, "\n"))
	return top.String() + box + "\n" + acts.String()
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
