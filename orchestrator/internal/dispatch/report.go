// Package dispatch is the push-and-reconcile chain of the control-plane
// writer (docs/designs/0001 D2, §4.3): the Dispatcher sends ticket work to
// the worker agent as an A2A task (SendMessage + ReturnImmediately +
// PushConfig), the push receiver registers task terminal states as they
// arrive, and the Pass beat reconciles running assignments against the task
// store so a lost push can never strand a ticket. Report documents carried in
// the task's status message drive the ticket state machine.
package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Report states (docs/designs/0001 §3): the worker's structured report
// vocabulary. The states drive the ticket state machine via PlanForReport.
const (
	ReportProgress = "progress" // work continues; the worker will run further turns
	ReportWaiting  = "waiting"  // paused for a person's input
	ReportReady    = "ready"    // work complete, result available (artifacts)
	ReportBlocked  = "blocked"  // cannot continue without outside action
	ReportFailed   = "failed"   // the work failed
	ReportDropped  = "dropped"  // the worker refused or abandoned the task
)

// ErrNoReport reports that a text carries no readable report document.
var ErrNoReport = errors.New("no report document")

// Report is the worker's structured report. Artifacts are the relative paths
// of produced files inside the workspace — the T7 patch contract reads them
// from the audit trail; dispatch only records them.
type Report struct {
	State     string   `json:"state"`
	Summary   string   `json:"summary,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// reportWire is the document the worker is asked to emit
// (docs/designs/0001 §3: prompt 约定 + 代码解析).
type reportWire struct {
	Kind      string   `json:"kind"`
	State     string   `json:"state"`
	Summary   string   `json:"summary,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// ParseReport extracts the worker's Report from a task status message text.
// The document may be the whole text or embedded in surrounding prose. A
// text without a readable six-state document is refused with ErrNoReport —
// the caller decides the fallback (ReportFromTaskState), never this parser.
func ParseReport(text string) (Report, error) {
	if doc, ok := parseReportDoc(strings.TrimSpace(text)); ok {
		return doc, nil
	}
	// Embedded in prose: try each '{'-anchored candidate, first match wins.
	for i := 0; i < len(text); {
		at := strings.IndexByte(text[i:], '{')
		if at < 0 {
			break
		}
		i += at
		if doc, ok := parseReportDoc(strings.TrimSpace(text[i:])); ok {
			return doc, nil
		}
		i++
	}
	return Report{}, fmt.Errorf("%w: %q", ErrNoReport, truncate(text))
}

func parseReportDoc(text string) (Report, bool) {
	var wire reportWire
	// Decode reads the first JSON value and tolerates trailing prose.
	if err := json.NewDecoder(strings.NewReader(text)).Decode(&wire); err != nil {
		return Report{}, false
	}
	switch wire.State {
	case ReportProgress, ReportWaiting, ReportReady, ReportBlocked, ReportFailed, ReportDropped:
		return Report{State: wire.State, Summary: wire.Summary, Artifacts: wire.Artifacts}, true
	default:
		// A JSON object that is not a six-state report is not a report.
		return Report{}, false
	}
}

// ReportFromTaskState derives the P0 fallback report from a bare A2A task
// terminal state when the worker emitted no readable report
// (docs/designs/0001 §3: P0 可从 task 终态+文本提取). Non-terminal states map
// to nothing: they are progress observations, not outcomes.
func ReportFromTaskState(taskState string) (string, bool) {
	switch taskState {
	case "TASK_STATE_COMPLETED":
		return ReportReady, true
	case "TASK_STATE_FAILED":
		return ReportFailed, true
	case "TASK_STATE_CANCELED", "TASK_STATE_REJECTED":
		return ReportDropped, true
	default:
		return "", false
	}
}

func truncate(s string) string {
	const max = 80
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
