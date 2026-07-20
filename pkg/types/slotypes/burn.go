package slotypes

// BurnSeverity classifies how a burn-rate alert should be routed.
//
//	page   -> fast burn, wake someone up
//	ticket -> slow burn, handle during working hours
type BurnSeverity string

const (
	BurnSeverityPage   BurnSeverity = "page"
	BurnSeverityTicket BurnSeverity = "ticket"
)
