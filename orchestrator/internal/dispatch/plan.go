package dispatch

// Plan is the state plan one terminal Report drives (docs/designs/0001 §3:
// Report 六态驱动工单状态机). The mapping is a pure semantic function: the
// assignment always settles (a terminal task cannot resume); the ticket
// moves per the report, or stays put when its work continues.
type Plan struct {
	AssignmentStatus string
	TicketStatus     string // "" = no ticket change
	Event            string
}

// PlanForReport maps the six report states onto the ticket machine:
//
//	progress → assignment ends, ticket stays running (Pass schedules the
//	           continuation via the follow-up scan);
//	ready    → assignment ends, ticket ends (result available; T7 posts it);
//	waiting  → assignment ends, ticket needs-attention (waiting for a person);
//	blocked  → assignment ends, ticket needs-attention;
//	failed   → assignment ends, ticket failed;
//	dropped  → assignment ends, ticket needs-attention.
func PlanForReport(report Report) Plan {
	switch report.State {
	case ReportProgress:
		return Plan{
			AssignmentStatus: "ended",
			Event:            "report.progress",
		}
	case ReportReady:
		return Plan{
			AssignmentStatus: "ended",
			TicketStatus:     "ended",
			Event:            "report.ready",
		}
	case ReportWaiting, ReportBlocked, ReportDropped:
		return Plan{
			AssignmentStatus: "ended",
			TicketStatus:     "needs-attention",
			Event:            "report." + report.State,
		}
	case ReportFailed:
		return Plan{
			AssignmentStatus: "ended",
			TicketStatus:     "failed",
			Event:            "report.failed",
		}
	default:
		// ParseReport refuses unknown states, so this is unreachable;
		// keeping the default conservative anyway.
		return Plan{AssignmentStatus: "ended", Event: "report." + report.State}
	}
}
