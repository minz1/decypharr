package repair

// Source says who started a recheck or a fix, which decides how much of the confirmation gate it may skip.
type Source string

const (
	// SourceOperator is a person acting through the UI or API. Their request is the confirmation, so the
	// two-observation gate is skipped; the breaker still applies unless they force.
	SourceOperator Source = "operator"
	// SourceWebhook is an automated caller such as Tautulli. It may probe, but it heals only what the gate already
	// confirmed, always through the breaker, and it can never force.
	SourceWebhook Source = "webhook"
)

// RecheckOptions says what a recheck may do about what it finds.
type RecheckOptions struct {
	// Fix heals what the recheck finds broken, within the limits of Source.
	Fix    bool
	Source Source
	// Force lets an operator heal past the breaker. A webhook's Force is ignored.
	Force bool
	// UnrestrictLink probes torrent files with a fresh provider link, so a hash the provider dropped reads broken.
	UnrestrictLink bool
	// Caller names who asked, for the log.
	Caller string
}

// FixOptions says how an operator's fix of already-broken entries is run.
type FixOptions struct {
	// Force heals past the breaker, including entries it is holding.
	Force  bool
	Caller string
}

// policy turns the request into the heal policy of its source.
func (o RecheckOptions) policy() healPolicy {
	if o.Source == SourceWebhook {
		return healPolicy{confirm: true}
	}
	return healPolicy{force: o.Force, caller: o.Caller}
}
