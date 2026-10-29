package localcontrol

// InstalledHandoffObservation is point-in-time redacted data, not readiness,
// ownership, runtime qualification, permission or a reservation of future quota.
// Its zero value is not an admitted observation.
type InstalledHandoffObservation struct{ state InstalledHandoffPresence }

type InstalledHandoffPresence string

const (
	InstalledHandoffMissingRuntimeDir   InstalledHandoffPresence = "missingRuntimeDir"
	InstalledHandoffAbsentNamespace     InstalledHandoffPresence = "absentNamespace"
	InstalledHandoffAttributedNamespace InstalledHandoffPresence = "attributedNamespace"
)

func (o InstalledHandoffObservation) Presence() InstalledHandoffPresence { return o.state }
func (InstalledHandoffObservation) String() string                       { return "installed handoff observation [redacted]" }
func (o InstalledHandoffObservation) GoString() string                   { return o.String() }
