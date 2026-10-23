package config

// API authentication settings (TAILOR_API_*). The token gates the mutating
// /api/* calls; TAILOR_API_PROTECT_READS extends the same Bearer check to the
// /api/* reads. Probes (/healthz, /livez), the Prometheus scrape (/metrics) and
// the UI shell (/) are never gated: the kubelet and the bundled vmsingle carry
// no token, and the UI must load to ask for one.

// EnvAPIProtectReads names the switch that puts GET/HEAD /api/* behind the token.
const EnvAPIProtectReads = "TAILOR_API_PROTECT_READS"

// apiProtectReads reads TAILOR_API_PROTECT_READS ("true" turns it on; anything
// else, including unset, keeps reads open for backwards compatibility).
func apiProtectReads() bool {
	return str(EnvAPIProtectReads, "false") == "true"
}

// AuthPosture describes the API auth configuration for the boot log and for
// error messages, without ever including the token itself.
func (c Config) AuthPosture() string {
	writes := "writes: open (no TAILOR_API_TOKEN)"
	switch {
	case c.APIToken != "":
		writes = "writes: Bearer token required"
	case c.APIAuthRequired:
		writes = "writes: disabled (TAILOR_API_AUTH_REQUIRED without TAILOR_API_TOKEN)"
	}
	reads := "reads: open"
	if c.APIProtectReads {
		reads = "reads: Bearer token required"
		if c.APIToken == "" {
			reads = "reads: disabled (" + EnvAPIProtectReads + " without TAILOR_API_TOKEN)"
		}
	}
	return "API auth — " + writes + "; " + reads
}
