package appproxy

// Status is what `modavd-proxyd status` prints and state.json holds. modavd shows it in `modavd proxy <i>`.
type Status struct {
	Version      string       `json:"version"` // the daemon's (modavd's) version
	Running      bool         `json:"running"`
	PID          int          `json:"pid"`
	StartedAt    string       `json:"started_at"` // RFC 3339, UTC
	UpdatedAt    string       `json:"updated_at"`
	ConfigSHA256 string       `json:"config_sha256"` // of the config.json it runs with
	Server       ServerStatus `json:"server"`
	Apps         []AppStatus  `json:"apps"`
	// Rules: RulesOK, RulesPartial (some went missing, being put back), RulesOff (none), RulesFailed (could not be
	// set up: RulesError says why).
	Rules      string   `json:"rules"`
	RulesError string   `json:"rules_error"`
	Counters   Counters `json:"counters"`
	Error      string   `json:"error"` // the daemon stopped because of this (bad config, ports taken); "" when running
}

const (
	RulesOK      = "ok"
	RulesPartial = "partial"
	RulesOff     = "off"
	RulesFailed  = "failed"
)

// ServerStatus is how the proxy has been answering.
type ServerStatus struct {
	URL string `json:"url"`
	// TCP: "ok", "failed" (the last attempt failed) or "unknown" (not tried yet).
	TCP string `json:"tcp"`
	// UDP: "ok", "unsupported" (an HTTP proxy, or UDP ASSOCIATE refused: the listed apps' UDP is dropped), "failed"
	// or "unknown".
	UDP         string `json:"udp"`
	LastError   string `json:"last_error"`
	LastErrorAt string `json:"last_error_at"`
	LastOKAt    string `json:"last_ok_at"`
}

// AppStatus is one listed package.
type AppStatus struct {
	Package string `json:"package"`
	UID     int    `json:"uid"` // 0: not installed
	// SharedWith are the other installed packages with this uid (android:sharedUserId); their traffic goes through the
	// proxy too.
	SharedWith []string `json:"shared_with,omitempty"`
}

// Counters count since the daemon started.
type Counters struct {
	TCP         int64 `json:"tcp"`          // connections relayed
	TCPFailed   int64 `json:"tcp_failed"`   // the proxy could not connect
	UDPSessions int64 `json:"udp_sessions"` // app sockets given a UDP association
	UDPDropped  int64 `json:"udp_dropped"`  // datagrams that could not go through the proxy
	DNS         int64 `json:"dns"`          // queries answered
	DNSFailed   int64 `json:"dns_failed"`   // answered SERVFAIL
	BytesUp     int64 `json:"bytes_up"`     // TCP and UDP payload, app to proxy
	BytesDown   int64 `json:"bytes_down"`
}
