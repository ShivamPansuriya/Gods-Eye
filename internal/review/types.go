package review

// Review is the full result. The map stage fills title/eli5/units; the hunt and
// verify stages fill Findings; Finalize/Merge tie them together.
type Review struct {
	Story   Story  `json:"story"`   // the PR as a whole: why, what, impact
	Title   string `json:"title"`   // ≤ 10 words: what the PR does, as a behaviour
	ELI5    string `json:"eli5"`    // ≤ 30 words, no jargon
	Verdict string `json:"verdict"` // approve | request_changes | needs_decision
	Units   []Unit `json:"units"`
	// What nobody (author, tests, this review) actually verified.
	NotChecked []string `json:"not_checked"`
	// Things to watch after deploy.
	WatchAfterDeploy []string `json:"watch_after_deploy"`

	// Filled by the pipeline, not the model.
	Findings []Finding `json:"findings,omitempty"`
	Pipeline *Pipeline `json:"pipeline,omitempty"`
}

// Story explains the PR as a whole, before any unit.
type Story struct {
	Why        string   `json:"why"`         // the problem or goal that made this PR necessary
	WhyBasis   string   `json:"why_basis"`   // stated (from description/issue) | inferred (from code/commits)
	What       string   `json:"what"`        // the overall change in plain words
	Before     string   `json:"before"`      // how the system behaved before, one sentence
	After      string   `json:"after"`       // how it behaves after, one sentence
	Impact     []Impact `json:"impact"`      // who is affected and how
	Focus      []string `json:"focus"`       // where the reviewer's attention matters most
	ReviewMins int      `json:"review_mins"` // estimated minutes for a human to review the units
}

// Impact is one effect of the PR on a group of people or systems.
type Impact struct {
	Who    string `json:"who"`    // users | admins | developers | operators | data
	Area   string `json:"area"`   // ≤ 4 words
	Change string `json:"change"` // ≤ 20 words
	Kind   string `json:"kind"`   // added | changed | removed | risk
}

// Unit is one behaviour change, made of one or more hunks.
type Unit struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Hunks      []string   `json:"hunks"`
	Tier       string     `json:"tier"` // human | agent | static
	Risk       string     `json:"risk"` // low | medium | high
	RiskReason string     `json:"risk_reason"`
	Brief      string     `json:"brief"`    // wrap key claims in [[double brackets]]
	Question   string     `json:"question"` // the one decision the reviewer must make here
	Flow       Flow       `json:"flow"`
	Notes      []LineNote `json:"notes"`
	Checks     []Check    `json:"checks"`
	Blast      []Blast    `json:"blast"`
	Ask        []string   `json:"ask"`

	Findings []string `json:"findings,omitempty"` // ids of confirmed findings in this unit
}

// Flow is a before/after chain of short steps.
type Flow struct {
	Before []Step `json:"before"`
	After  []Step `json:"after"`
}

// Step is one box in the flow.
type Step struct {
	Label string `json:"label"`
	Edge  string `json:"edge"`
	State string `json:"state"` // same | added | removed | changed
	Hunk  string `json:"hunk"`
	Line  int    `json:"line"`
}

// LineNote explains one important line.
type LineNote struct {
	Hunk string `json:"hunk"`
	Line int    `json:"line"`
	Code string `json:"code"`
	Note string `json:"note"`
}

// Check is a done-when criterion with evidence.
type Check struct {
	Criterion string   `json:"criterion"`
	Status    string   `json:"status"` // done | not_done | no_evidence
	Detail    string   `json:"detail"`
	Source    string   `json:"source"` // pr | issue | guidance | inferred
	Evidence  Evidence `json:"evidence"`
	Finding   string   `json:"finding,omitempty"`  // id of the finding that verified a not_done
	Disputed  bool     `json:"disputed,omitempty"` // verifiers rejected the not_done claim
}

// Evidence points at the thing that proves (or fails to prove) a check.
type Evidence struct {
	Kind string `json:"kind"` // code | test | doc | gap
	Hunk string `json:"hunk"`
	Line int    `json:"line"`
	Ref  string `json:"ref"`
}

// Blast is something outside the diff that this change affects.
type Blast struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Basis  string `json:"basis"` // seen_in_diff | seen_in_repo | inferred
}

// Loc is a file position.
type Loc struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// Finding is a candidate bug from a hunter (or a not_done check), and what the
// verifiers decided about it.
type Finding struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Quote      string `json:"quote"`
	Severity   string `json:"severity"` // important | minor | nit (after the vote)
	Category   string `json:"category"`
	Scenario   string `json:"scenario"`
	Fix        string `json:"fix"`
	BugClass   string `json:"bug_class"`
	AlsoAt     []Loc  `json:"also_at"`
	Confidence string `json:"confidence"`

	Source string `json:"source"`         // hunter lens, or "check"
	Hunk   string `json:"hunk,omitempty"` // hunk containing the line, if any
	Unit   string `json:"unit,omitempty"`
	InDiff bool   `json:"inDiff"` // the line is added or changed by this PR

	Votes  []Vote `json:"votes,omitempty"`
	Status string `json:"status"` // confirmed | rejected | nit | unverifiable | fixed | skipped
	// Introduced is false when the verifiers judged the bug pre-existing.
	Introduced bool   `json:"introduced"`
	Round      int    `json:"round"`
	Why        string `json:"why,omitempty"`      // short reason for rejected / unverifiable
	CheckRef   string `json:"checkRef,omitempty"` // "unit:check" index when this came from a not_done check
}

// Vote is one verifier's verdict.
type Vote struct {
	Lens       string `json:"lens"`
	Real       bool   `json:"real"`
	Introduced bool   `json:"introduced"`
	Severity   string `json:"severity"`
	Confidence string `json:"confidence"`
	Reason     string `json:"reason"`
}

// Pipeline describes how the review was produced (shown on the page).
type Pipeline struct {
	Depth        string                `json:"depth"`
	Round        int                   `json:"round"`
	Since        string                `json:"since,omitempty"` // previous reviewed sha for incremental rounds
	Hunters      int                   `json:"hunters"`
	Voters       int                   `json:"voters"`
	Candidates   int                   `json:"candidates"`
	Duplicates   int                   `json:"duplicates"`
	Unverifiable int                   `json:"unverifiable"`
	OutsideDelta int                   `json:"outsideDelta,omitempty"` // re-review findings outside the new changes, dropped
	Confirmed    int                   `json:"confirmed"`
	PreExisting  int                   `json:"preExisting"`
	Rejected     int                   `json:"rejected"`
	Nits         int                   `json:"nits"`
	Minor        int                   `json:"minor"`
	Fixed        int                   `json:"fixed"`
	Symbols      int                   `json:"symbols"`
	Workspace    bool                  `json:"workspace"`
	VerifyCmd    string                `json:"verifyCmd,omitempty"`
	VerifyExit   int                   `json:"verifyExit,omitempty"`
	Seconds      float64               `json:"seconds"`
	Usage        map[string]StageUsage `json:"usage"`
	CostUSD      float64               `json:"costUsd"`
	Notice       string                `json:"notice,omitempty"`
	Errors       []string              `json:"errors,omitempty"`
}

// StageUsage mirrors agent.StageUsage for the report.
type StageUsage struct {
	Calls      int     `json:"calls"`
	Input      int64   `json:"input"`
	CacheWrite int64   `json:"cacheWrite"`
	CacheRead  int64   `json:"cacheRead"`
	Output     int64   `json:"output"`
	CostUSD    float64 `json:"costUsd"`
	Seconds    float64 `json:"seconds"`
}
