package filesystem

import (
	"fmt"
	"time"
)

// Report summarizes a completed scan operation.
type Report struct {
	TotalDiscovered  int64         `json:"total_discovered"`
	TotalAccepted    int64         `json:"total_accepted"`
	TotalSkipped     int64         `json:"total_skipped"`
	TotalDuplicates  int64         `json:"total_duplicates"`
	DirsSkipped      int64         `json:"dirs_skipped"`
	Warnings         int           `json:"warnings"`
	Duration         time.Duration `json:"duration"`
	ScanScopes       int           `json:"scan_scopes"`
}

// String returns a human-readable summary of the scan report.
func (r *Report) String() string {
	return fmt.Sprintf("scan report: discovered=%d accepted=%d skipped=%d duplicates=%d dirs_skipped=%d warnings=%d duration=%s",
		r.TotalDiscovered, r.TotalAccepted, r.TotalSkipped, r.TotalDuplicates, r.DirsSkipped, r.Warnings, r.Duration)
}

// FromDiscovery converts a DiscoveryResult to a Report.
func FromDiscovery(d *DiscoveryResult) *Report {
	return &Report{
		TotalDiscovered: d.TotalDiscovered,
		TotalAccepted:   d.TotalAccepted,
		TotalSkipped:    d.TotalSkipped,
		TotalDuplicates: d.TotalDuplicates,
		DirsSkipped:     d.DirsSkipped,
		Warnings:        int(d.Warnings),
		Duration:        d.Duration,
		ScanScopes:      0, // will be set by caller
	}
}
