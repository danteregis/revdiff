package ui

import (
	"fmt"
	"log"

	tea "github.com/charmbracelet/bubbletea"
)

// outputState holds transient feedback for the O in-session output flush.
// hint is a status-bar message cleared on the next key press, mirroring
// reloadState.hint.
type outputState struct {
	hint string // transient status-bar message; cleared on next key press
}

type postFlushFinishedMsg struct {
	err          error
	successHint  string
	failureHint  string
	restoreMouse bool
	deliver      bool // mark the flushed annotations delivered once the command succeeds
}

// handleFlushOutput exports the pending annotations (open and not yet
// delivered) through the configured output file and/or post-flush command
// without exiting. Without a review session the store is never mutated, so
// annotations persist in-session and can be re-flushed. With a session the
// flushed annotations are marked delivered — when the file is written, or for
// a command-only flush when the command succeeds — so the next flush or the
// exit output carries only what was added or edited since. Feedback is
// reported through output.hint.
func (m Model) handleFlushOutput() (tea.Model, tea.Cmd) {
	n := m.store.PendingCount()
	if n == 0 {
		m.output.hint = "No annotations to flush"
		if m.store.Count() > 0 {
			m.output.hint = "No new annotations to flush"
		}
		return m, nil
	}
	if m.cfg.outputPath == "" && m.postFlushHook == nil {
		m.output.hint = "Output flush requires -o/--output or --post-flush-command"
		return m, nil
	}
	noun := "annotations"
	if n == 1 {
		noun = "annotation"
	}

	var content, writtenHint string
	if m.cfg.outputPath != "" {
		var err error
		content, err = m.store.WriteFile(m.cfg.outputPath)
		if err != nil {
			log.Printf("[WARN] flush annotations to output: %v", err)
			m.output.hint = "Flush failed"
			return m, nil
		}
		writtenHint = fmt.Sprintf("Wrote %d %s to output file", n, noun)
		m.MarkDelivered()
	} else {
		content = m.store.FormatOutput()
	}

	if m.postFlushHook == nil {
		m.output.hint = writtenHint
		return m, nil
	}

	runningHint := fmt.Sprintf("Running post-flush command with %d %s", n, noun)
	successHint := fmt.Sprintf("Ran post-flush command with %d %s", n, noun)
	failureHint := "Post-flush command failed"
	if writtenHint != "" {
		runningHint = writtenHint + "; running post-flush command"
		successHint = writtenHint + " and ran post-flush command"
		failureHint = writtenHint + "; post-flush command failed"
	}
	cmd := m.postFlushHook.Prepare(content)
	m.output.hint = runningHint
	deliver := writtenHint == ""
	return m, tea.ExecProcess(cmd, func(runErr error) tea.Msg {
		return postFlushFinishedMsg{
			err:          runErr,
			successHint:  successHint,
			failureHint:  failureHint,
			restoreMouse: m.cfg.mouseTracking,
			deliver:      deliver,
		}
	})
}

func (m Model) handlePostFlushFinished(msg postFlushFinishedMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	if msg.restoreMouse {
		cmd = tea.EnableMouseCellMotion
	}
	if msg.err != nil {
		log.Printf("[WARN] post-flush command failed: %v", msg.err)
		m.output.hint = msg.failureHint
		return m, cmd
	}
	m.output.hint = msg.successHint
	if msg.deliver {
		m.MarkDelivered()
		m.invalidateRenderCaches()
		if m.file.name != "" {
			m.layout.viewport.SetContent(m.renderDiff())
		}
	}
	return m, cmd
}
