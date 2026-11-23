package services

import "log/slog"

// PerfProbe receives results from the frontend's scroll benchmark. It is
// only registered when NEXORA_PERF is set (see main.go).
type PerfProbe struct{ log *slog.Logger }

func NewPerfProbe(log *slog.Logger) *PerfProbe { return &PerfProbe{log: log} }

// Report logs one benchmark result.
func (p *PerfProbe) Report(result string) {
	p.log.Info("perf probe", "result", result)
}
