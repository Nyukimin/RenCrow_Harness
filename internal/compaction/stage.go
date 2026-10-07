package compaction

import (
	"fmt"
	"sync"

	harness "github.com/Nyukimin/RenCrow_Harness"
	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/contextplan"
	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

const (
	stageDataPrefix = "RENCROW_STAGE_DATA_V1\n"
	// MaxDatasetBytes bounds the CJ1 of a stage's data. A dataset that does not fit is
	// not cut: the Normal compaction does not happen.
	MaxDatasetBytes = 8 << 20

	selectionFormat = "rencrow-selection-data/v1"
	summaryFormat   = "rencrow-summary-data/v1"
)

// Piece is one presented fragment of a text: an instruction (Selection, Summary) or a
// piece of Work (Summary), at most MaxPieceBytes.
type Piece struct {
	Handle               string `json:"handle"`
	Origin               string `json:"origin"`
	Sequence             int64  `json:"sequence"`
	ChunkIndex           int64  `json:"chunk_index"`
	ChunkCount           int64  `json:"chunk_count"`
	Text                 string `json:"text"`
	ContinuingConstraint bool   `json:"continuing_constraint"`
}

// CompletionLink is a candidate completion offered to Selection: a terminal
// process.exec pair that the one instruction fragment named by TargetHandle explicitly
// names.
type CompletionLink struct {
	Handle       string `json:"handle"`
	TargetHandle string `json:"target_handle"`
	CallText     string `json:"call_text"`
	OutputText   string `json:"output_text"`
	ExitCode     int64  `json:"exit_code"`
}

// SelectionDataset is the data of the Selection stage (stage_data.schema.json).
type SelectionDataset struct {
	FormatVersion    string           `json:"format_version"`
	PresentedSources []Piece          `json:"presented_sources"`
	CompletionLinks  []CompletionLink `json:"completion_links"`
}

// Excerpt is the text of one byte range of an Observation, as presented.
type Excerpt struct {
	Range contextplan.Range `json:"range"`
	Text  string            `json:"text"`
}

// ObservationData is one Observation presented to Summary.
type ObservationData struct {
	Handle          string    `json:"handle"`
	Tool            string    `json:"tool"`
	Sequence        int64     `json:"sequence"`
	TotalBytes      int64     `json:"total_bytes"`
	Partial         bool      `json:"partial"`
	CaptureComplete bool      `json:"capture_complete"`
	Excerpts        []Excerpt `json:"excerpts"`
}

// Completion is a verified completion offered to Summary.
type Completion struct {
	Handle            string   `json:"handle"`
	InstructionHandle []string `json:"instruction_handles"`
	CallText          string   `json:"call_text"`
	OutputText        string   `json:"output_text"`
	ExitCode          int64    `json:"exit_code"`
}

// ProtectedData is state that is kept and never summarized away: it is shown so it can
// be taken into account, with the text when there is a text.
type ProtectedData struct {
	Kind        string  `json:"kind"`
	Description string  `json:"description"`
	Text        *string `json:"text"`
	TotalBytes  int64   `json:"total_bytes"`
	Partial     bool    `json:"partial"`
}

// SummaryText is a Summary as the next Summary is given it: the records without their
// source handles.
type SummaryText struct {
	CurrentWork  []textItem     `json:"current_work"`
	Decisions    []decisionItem `json:"decisions"`
	Verification []verifyItem   `json:"verification"`
	OpenItems    []textItem     `json:"open_items"`
	NextSteps    []textItem     `json:"next_steps"`
}

type textItem struct {
	Text string `json:"text"`
}
type decisionItem struct {
	Text   string `json:"text"`
	Reason string `json:"reason"`
}
type verifyItem struct {
	Text  string `json:"text"`
	State string `json:"state"`
}

// PriorSummary is the one previous accepted Summary the Summary stage may continue from.
type PriorSummary struct {
	Handle  string      `json:"handle"`
	Summary SummaryText `json:"summary"`
}

// SummaryDataset is the data of the Summary stage.
type SummaryDataset struct {
	FormatVersion  string            `json:"format_version"`
	Work           []Piece           `json:"work"`
	Observations   []ObservationData `json:"observations"`
	Instructions   []Piece           `json:"instructions"`
	PriorSummaries []PriorSummary    `json:"prior_summaries"`
	Completions    []Completion      `json:"completions"`
	ProtectedState []ProtectedData   `json:"protected_state"`
}

var (
	promptMu    sync.Mutex
	promptCache = map[string]string{}
)

// stagePrompt is the whole text of the stage's prompt file: the only system message a
// stage request has.
func stagePrompt(stage string) (string, error) {
	name := map[string]string{modelport.StageSelection: "prompts/instruction_selection.md", modelport.StageSummary: "prompts/work_summary.md"}[stage]
	if name == "" {
		return "", fmt.Errorf("compaction: %q is not a compaction stage", stage)
	}
	promptMu.Lock()
	defer promptMu.Unlock()
	if t, ok := promptCache[name]; ok {
		return t, nil
	}
	b, err := harness.Prompts.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("compaction: the stage prompt is not embedded: %w", err)
	}
	promptCache[name] = string(b)
	return string(b), nil
}

// StageMessages is the whole prompt of one stage request (STAGE_DATA section 1): the
// stage's prompt file as the system message and the data, as one user message that
// opens with its marker and carries the data in CJ1. No persona, history or Tool
// declaration is added. The data is checked against its schema and the byte bound
// first; the returned digest is D("rencrow-stage-data/v1", data).
func StageMessages(stage string, dataset any) (msgs []modelport.ChatMessage, digest string, err error) {
	def := ""
	switch dataset.(type) {
	case SelectionDataset, *SelectionDataset:
		def = "SelectionDataset"
		if stage != modelport.StageSelection {
			return nil, "", fmt.Errorf("compaction: selection data is not for the %s stage", stage)
		}
	case SummaryDataset, *SummaryDataset:
		def = "SummaryDataset"
		if stage != modelport.StageSummary {
			return nil, "", fmt.Errorf("compaction: summary data is not for the %s stage", stage)
		}
	default:
		return nil, "", fmt.Errorf("compaction: unknown stage data")
	}
	val, err := toValue(dataset)
	if err != nil {
		return nil, "", semanticf("the stage data cannot be encoded")
	}
	if err := schemacheck.Validate(schemacheck.StageData, def, val); err != nil {
		return nil, "", semanticf("the stage data does not satisfy its schema: %v", err)
	}
	body, err := canon.Encode(val)
	if err != nil {
		return nil, "", semanticf("the stage data cannot be encoded")
	}
	if len(body) > MaxDatasetBytes {
		return nil, "", semanticf("the stage data is over %d bytes", MaxDatasetBytes)
	}
	prompt, err := stagePrompt(stage)
	if err != nil {
		return nil, "", err
	}
	digest, err = canon.D("rencrow-stage-data/v1", val)
	if err != nil {
		return nil, "", err
	}
	return []modelport.ChatMessage{modelport.System(prompt), modelport.User(stageDataPrefix + string(body))}, digest, nil
}

// decodeStageOutput reads what a stage answered: one JSON object, strictly (no
// duplicate key, no BOM, nothing after it), then checks it against the stage's answer
// schema. A reply that is not exactly that is ErrSemantic.
func decodeStageOutput(schema, text string) (any, error) {
	v, err := strictjson.Decode([]byte(text))
	if err != nil {
		return nil, semanticf("the stage's answer is not one strict JSON value")
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, semanticf("the stage's answer is not a JSON object")
	}
	if err := schemacheck.Validate(schema, "", v); err != nil {
		return nil, semanticf("the stage's answer does not satisfy its schema: %v", err)
	}
	return v, nil
}
