package review

// Review is the model's structured output. Every field is short by design: the
// page is built from these fields, so there is nowhere for long prose to go.
type Review struct {
	Title   string `json:"title"`   // ≤ 10 words: what the PR does, as a behaviour
	ELI5    string `json:"eli5"`    // ≤ 30 words, no jargon
	Verdict string `json:"verdict"` // approve | request_changes | needs_decision
	Units   []Unit `json:"units"`
	// What nobody (author, tests, this review) actually verified.
	NotChecked []string `json:"not_checked"`
	// Things to watch after deploy.
	WatchAfterDeploy []string `json:"watch_after_deploy"`
}

// Unit is one behaviour change, made of one or more hunks.
type Unit struct {
	ID         string     `json:"id"`          // U1, U2, ...
	Title      string     `json:"title"`       // ≤ 8 words, a behaviour: "Index writes retry when the DB is busy"
	Hunks      []string   `json:"hunks"`       // H ids from the annotated diff
	Tier       string     `json:"tier"`        // human | agent | static
	Risk       string     `json:"risk"`        // low | medium | high
	RiskReason string     `json:"risk_reason"` // ≤ 12 words
	Brief      string     `json:"brief"`       // ≤ 35 words; wrap key claims in [[double brackets]]
	Flow       Flow       `json:"flow"`
	Notes      []LineNote `json:"notes"`
	Checks     []Check    `json:"checks"`
	Blast      []Blast    `json:"blast"`
	Ask        []string   `json:"ask"` // concrete change requests, ≤ 25 words each; empty if none
}

// Flow is a before/after chain of short steps (diffmap-style).
type Flow struct {
	Before []Step `json:"before"`
	After  []Step `json:"after"`
}

// Step is one box in the flow. State marks whether it's new, removed or unchanged.
type Step struct {
	Label string `json:"label"` // ≤ 4 words or a function name
	Edge  string `json:"edge"`  // label on the arrow leaving this step, ≤ 3 words, may be ""
	State string `json:"state"` // same | added | removed | changed
	Hunk  string `json:"hunk"`  // H id this step lives in, or ""
	Line  int    `json:"line"`  // new-file line number, or 0
}

// LineNote explains one important line ("Go in this unit").
type LineNote struct {
	Hunk string `json:"hunk"`
	Line int    `json:"line"` // new-file line number (or old number for deleted lines)
	Code string `json:"code"` // the code, trimmed, ≤ 60 chars
	Note string `json:"note"` // ≤ 20 words, plain language
}

// Check is a done-when criterion with evidence.
type Check struct {
	Criterion string   `json:"criterion"` // ≤ 10 words
	Status    string   `json:"status"`    // done | not_done | no_evidence
	Detail    string   `json:"detail"`    // ≤ 25 words
	Source    string   `json:"source"`    // where the criterion came from: pr | issue | guidance | inferred
	Evidence  Evidence `json:"evidence"`
}

// Evidence points at the thing that proves (or fails to prove) a check.
type Evidence struct {
	Kind string `json:"kind"` // code | test | doc | gap
	Hunk string `json:"hunk"` // "" when none
	Line int    `json:"line"`
	Ref  string `json:"ref"` // ≤ 8 words, e.g. "writer.go lines 29-36" or "no test hits busy path"
}

// Blast is something outside the diff that this change affects.
type Blast struct {
	Title  string `json:"title"`  // ≤ 8 words
	Detail string `json:"detail"` // ≤ 25 words
	Basis  string `json:"basis"`  // seen_in_diff | inferred
}
