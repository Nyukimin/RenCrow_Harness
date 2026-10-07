package modelport

// StateSet is a set of generation states.
type StateSet uint8

// The members of a StateSet.
const (
	InNotStarted StateSet = 1 << iota
	InTerminal
	InUnknown
	InAny = InNotStarted | InTerminal | InUnknown
)

// Has reports whether the generation state is in the set. A string that is none of
// the three states is in no set.
func (s StateSet) Has(state string) bool {
	switch state {
	case StateNotStarted:
		return s&InNotStarted != 0
	case StateTerminal:
		return s&InTerminal != 0
	case StateUnknown:
		return s&InUnknown != 0
	}
	return false
}

// CodeFact is what ERROR_MAPPING lets a normalized code say: the generation states it
// can come with, and whether the table can ever mark it retryable. The retry itself
// is decided elsewhere (the kernel's F31); a "retryable" here is only the most a
// Gateway's own flag may say.
type CodeFact struct {
	States StateSet
	Retry  bool
}

// codeFacts is ERROR_MAPPING section 2 as data. A code that is not here is not one
// the Harness maps. CANCELLED and PERMIT_REVOKED are the Harness's own codes, and a
// Gateway is not taken to state them.
var codeFacts = map[string]CodeFact{
	CodeQueueTimeout:          {InNotStarted, true},
	CodeConnectFailed:         {InNotStarted, true},
	CodeRateLimited:           {InNotStarted, true},
	CodeUpstreamTransient:     {InTerminal, true},
	CodeReasoningOnly:         {InTerminal, true},
	CodeEmptyFinalContent:     {InTerminal, true},
	CodeRawToolMarkup:         {InTerminal | InUnknown, true},
	CodeOutputSchemaInvalid:   {InTerminal | InUnknown, true},
	CodeOutputDegenerate:      {InTerminal | InUnknown, false},
	CodeModelUnavailable:      {InNotStarted | InUnknown, false},
	CodeOutcomeUnknown:        {InUnknown, false},
	CodeContractFailed:        {InAny, false},
	CodeContextLimit:          {InNotStarted | InTerminal, false},
	CodeBudgetUnverified:      {InNotStarted, false},
	CodeBindingChanged:        {InNotStarted, false},
	CodeInputDigestMismatch:   {InNotStarted, false},
	CodeRequestDigestMismatch: {InNotStarted, false},
	CodeUnsupportedContract:   {InNotStarted, false},
	CodeUnsupportedRecovery:   {InNotStarted, false},
	CodeAuthFailed:            {InNotStarted, false},
	CodeLength:                {InTerminal, false},
	CodeIncomplete:            {InTerminal | InUnknown, false},
	CodeRefused:               {InTerminal, false},
}

// FactOf returns what the mapping says of a normalized code. The second result is
// false for a code the mapping does not list.
func FactOf(code string) (CodeFact, bool) {
	f, ok := codeFacts[code]
	return f, ok
}
