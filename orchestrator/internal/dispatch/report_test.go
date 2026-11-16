package dispatch_test

import (
	"errors"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
)

func TestParseReport(t *testing.T) {
	t.Run("Given a worker's report document, when it is parsed, assert that all six report states decode with their summary and artifacts", func(t *testing.T) {
		scenarios := []struct {
			text      string
			wantState string
		}{
			{`{"kind":"report","state":"progress","summary":"editing handler.go"}`, dispatch.ReportProgress},
			{`{"kind":"report","state":"waiting","summary":"need the API key rotated"}`, dispatch.ReportWaiting},
			{`{"kind":"report","state":"ready","summary":"patch at patches/ticket-1-1.patch","artifacts":["patches/ticket-1-1.patch"]}`, dispatch.ReportReady},
			{`{"kind":"report","state":"blocked","summary":"CI is down"}`, dispatch.ReportBlocked},
			{`{"kind":"report","state":"failed","summary":"tests did not compile"}`, dispatch.ReportFailed},
			{`{"kind":"report","state":"dropped","summary":"scope exceeds the mandate"}`, dispatch.ReportDropped},
		}
		for _, sc := range scenarios {
			report, err := dispatch.ParseReport(sc.text)
			if err != nil {
				t.Fatalf("ParseReport(%s) failed: %v", sc.text, err)
			}
			if report.State != sc.wantState {
				t.Fatalf("report state = %q, want %q", report.State, sc.wantState)
			}
		}
	})

	t.Run("Given a report with summary and artifact paths, when it is parsed, assert that the payload fields are carried verbatim", func(t *testing.T) {
		report, err := dispatch.ParseReport(`{"kind":"report","state":"ready","summary":"PR ready","artifacts":["patches/t1-1.patch","patches/t1-2.patch"]}`)
		if err != nil {
			t.Fatalf("ParseReport failed: %v", err)
		}
		if report.Summary != "PR ready" {
			t.Fatalf("summary = %q, want the worker's summary", report.Summary)
		}
		if len(report.Artifacts) != 2 || report.Artifacts[0] != "patches/t1-1.patch" {
			t.Fatalf("artifacts = %v, want the worker's artifact paths (T7's patch contract)", report.Artifacts)
		}
	})

	t.Run("Given prose wrapping a report document, when it is parsed, assert that the embedded report is found", func(t *testing.T) {
		report, err := dispatch.ParseReport("Worker turn complete.\n{\"kind\":\"report\",\"state\":\"progress\",\"summary\":\"half done\"}\n(next turn follows)")
		if err != nil {
			t.Fatalf("ParseReport failed: %v", err)
		}
		if report.State != dispatch.ReportProgress || report.Summary != "half done" {
			t.Fatalf("report = %+v, want the embedded progress report", report)
		}
	})

	t.Run("Given a document naming an unknown state, when it is parsed, assert that parsing refuses it", func(t *testing.T) {
		if _, err := dispatch.ParseReport(`{"kind":"report","state":"victory"}`); !errors.Is(err, dispatch.ErrNoReport) {
			t.Fatalf("err = %v, want ErrNoReport", err)
		}
	})

	t.Run("Given text without any report document, when it is parsed, assert that parsing reports no report", func(t *testing.T) {
		for _, text := range []string{"", "just some prose", `{\"broken\"}`, `{"state":"ready"`} {
			if _, err := dispatch.ParseReport(text); !errors.Is(err, dispatch.ErrNoReport) {
				t.Fatalf("ParseReport(%q) err = %v, want ErrNoReport", text, err)
			}
		}
	})
}

func TestReportFromTaskState(t *testing.T) {
	t.Run("Given a terminal task without a parseable report, when the report is derived, assert that the A2A terminal state maps onto the report vocabulary", func(t *testing.T) {
		scenarios := []struct {
			taskState string
			want      string
		}{
			{"TASK_STATE_COMPLETED", dispatch.ReportReady},
			{"TASK_STATE_FAILED", dispatch.ReportFailed},
			{"TASK_STATE_CANCELED", dispatch.ReportDropped},
			{"TASK_STATE_REJECTED", dispatch.ReportDropped},
		}
		for _, sc := range scenarios {
			got, ok := dispatch.ReportFromTaskState(sc.taskState)
			if !ok {
				t.Fatalf("ReportFromTaskState(%s) reported no mapping", sc.taskState)
			}
			if got != sc.want {
				t.Fatalf("task state %s mapped to %q, want %q", sc.taskState, got, sc.want)
			}
		}
	})

	t.Run("Given a non-terminal task state, when the report is derived, assert that no mapping is offered", func(t *testing.T) {
		for _, state := range []string{"TASK_STATE_WORKING", "TASK_STATE_SUBMITTED", "TASK_STATE_INPUT_REQUIRED", "", "TASK_STATE_UNSPECIFIED"} {
			if _, ok := dispatch.ReportFromTaskState(state); ok {
				t.Fatalf("task state %q must not map to a report", state)
			}
		}
	})
}
