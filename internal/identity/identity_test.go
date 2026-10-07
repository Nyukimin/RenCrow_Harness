package identity_test

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nyukimin/RenCrow_Harness/internal/identity"
)

// idCase is the independent expectation for one Canonical ID: schema $defs name,
// prefix, and the typed constructor/parser pair.
type idCase struct {
	typeName string
	prefix   string
	create   func() string
	parse    func(string) error
	// foreign parses with another ID type and must always fail.
	foreign func(string) error
}

func cases() []idCase {
	return []idCase{
		{"TraceID", "trc_", func() string { return identity.NewTraceID().String() }, func(s string) error { _, err := identity.ParseTraceID(s); return err }, nil},
		{"EventID", "evt_", func() string { return identity.NewEventID().String() }, func(s string) error { _, err := identity.ParseEventID(s); return err }, nil},
		{"SessionID", "ses_", func() string { return identity.NewSessionID().String() }, func(s string) error { _, err := identity.ParseSessionID(s); return err }, nil},
		{"ThreadID", "thr_", func() string { return identity.NewThreadID().String() }, func(s string) error { _, err := identity.ParseThreadID(s); return err }, nil},
		{"TurnID", "turn_", func() string { return identity.NewTurnID().String() }, func(s string) error { _, err := identity.ParseTurnID(s); return err }, nil},
		{"MessageID", "msg_", func() string { return identity.NewMessageID().String() }, func(s string) error { _, err := identity.ParseMessageID(s); return err }, nil},
		{"TaskID", "tsk_", func() string { return identity.NewTaskID().String() }, func(s string) error { _, err := identity.ParseTaskID(s); return err }, nil},
		{"RunID", "run_", func() string { return identity.NewRunID().String() }, func(s string) error { _, err := identity.ParseRunID(s); return err }, nil},
		{"ActionID", "act_", func() string { return identity.NewActionID().String() }, func(s string) error { _, err := identity.ParseActionID(s); return err }, nil},
		{"AttemptID", "att_", func() string { return identity.NewAttemptID().String() }, func(s string) error { _, err := identity.ParseAttemptID(s); return err }, nil},
		{"RequestID", "req_", func() string { return identity.NewRequestID().String() }, func(s string) error { _, err := identity.ParseRequestID(s); return err }, nil},
		{"ResponseID", "rsp_", func() string { return identity.NewResponseID().String() }, func(s string) error { _, err := identity.ParseResponseID(s); return err }, nil},
		{"EvidenceID", "evd_", func() string { return identity.NewEvidenceID().String() }, func(s string) error { _, err := identity.ParseEvidenceID(s); return err }, nil},
		{"CheckpointID", "ckp_", func() string { return identity.NewCheckpointID().String() }, func(s string) error { _, err := identity.ParseCheckpointID(s); return err }, nil},
		{"ReceiptID", "rcp_", func() string { return identity.NewReceiptID().String() }, func(s string) error { _, err := identity.ParseReceiptID(s); return err }, nil},
		{"QueueItemID", "qit_", func() string { return identity.NewQueueItemID().String() }, func(s string) error { _, err := identity.ParseQueueItemID(s); return err }, nil},
	}
}

func schemaIDPatterns(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("../../schemas/protocol.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	idShape := regexp.MustCompile(`^\^[a-z]+_\[0-9a-f\]\{8\}-`)
	out := map[string]string{}
	for name, def := range doc.Defs {
		if idShape.MatchString(def.Pattern) {
			out[name] = def.Pattern
		}
	}
	return out
}

func TestNewProducesPrefixedUUIDv7(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.typeName, func(t *testing.T) {
			s := c.create()
			if !strings.HasPrefix(s, c.prefix) {
				t.Fatalf("%q lacks prefix %q", s, c.prefix)
			}
			u, err := uuid.Parse(strings.TrimPrefix(s, c.prefix))
			if err != nil {
				t.Fatal(err)
			}
			if u.Version() != 7 || u.Variant() != uuid.RFC4122 {
				t.Fatalf("got version %d variant %v, want UUIDv7 RFC4122", u.Version(), u.Variant())
			}
			if err := c.parse(s); err != nil {
				t.Fatalf("own output rejected: %v", err)
			}
		})
	}
}

func TestNewIsUniqueAndTimeOrdered(t *testing.T) {
	const n = 20000
	seen := make(map[string]struct{}, n)
	prev := ""
	for i := 0; i < n; i++ {
		s := identity.NewEvidenceID().String()
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate id %s", s)
		}
		seen[s] = struct{}{}
		if prev != "" && s <= prev {
			t.Fatalf("not monotonically increasing: %s after %s", s, prev)
		}
		prev = s
	}
}

func TestParseMatchesSchemaPatterns(t *testing.T) {
	patterns := schemaIDPatterns(t)
	want := map[string]string{}
	for _, c := range cases() {
		want[c.typeName] = c.prefix
	}
	if len(patterns) != len(want) {
		t.Fatalf("schema has %d ID defs, identity covers %d", len(patterns), len(want))
	}
	const body = "[0-9a-f]{8}-[0-9a-f]{4}-[57][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
	probes := []string{
		"00000000-0000-7000-8000-000000000001", // valid v7
		"00000000-0000-5000-b000-000000000001", // valid v5
		"018f1c2d-3e4f-7a5b-8c6d-7e8f9a0b000a",
		"00000000-0000-4000-8000-000000000001", // v4
		"00000000-0000-1000-8000-000000000001", // v1
		"00000000-0000-0000-0000-000000000000", // nil
		"00000000-0000-7000-7000-000000000001", // variant 7
		"00000000-0000-7000-c000-000000000001", // variant c
		"00000000-0000-7000-8000-00000000000A", // uppercase
		"{00000000-0000-7000-8000-000000000001}",
		"urn:uuid:00000000-0000-7000-8000-000000000001",
		"00000000000070008000000000000001",
		"00000000-0000-7000-8000-000000000001\n",
		" 00000000-0000-7000-8000-000000000001",
		"",
	}
	for _, c := range cases() {
		pat, ok := patterns[c.typeName]
		if !ok {
			t.Fatalf("schema has no %s", c.typeName)
		}
		if pat != "^"+c.prefix+body {
			t.Fatalf("%s pattern drifted: %s", c.typeName, pat)
		}
		re := regexp.MustCompile(pat)
		for _, probe := range probes {
			for _, input := range []string{c.prefix + probe, probe, strings.ToUpper(c.prefix) + probe, "xxx_" + probe} {
				if got, want := c.parse(input) == nil, re.MatchString(input); got != want {
					t.Errorf("%s %q: Parse ok=%v but schema pattern match=%v", c.typeName, input, got, want)
				}
			}
		}
	}
}

func TestParseRejectsForeignPrefix(t *testing.T) {
	all := cases()
	for i, c := range all {
		other := all[(i+1)%len(all)]
		if err := c.parse(other.create()); err == nil {
			t.Errorf("%s accepted %s", c.typeName, other.typeName)
		}
		if err := c.parse(strings.ToUpper(c.create())); err == nil {
			t.Errorf("%s accepted uppercase text", c.typeName)
		}
	}
	// Prefix-only and double prefix are not IDs.
	if _, err := identity.ParseTaskID("tsk_"); err == nil {
		t.Error("prefix only accepted")
	}
	if _, err := identity.ParseTaskID("tsk_tsk_00000000-0000-7000-8000-000000000001"); err == nil {
		t.Error("double prefix accepted")
	}
}

func TestValidateAndZero(t *testing.T) {
	var zero identity.TaskID
	if !zero.IsZero() || zero.Validate() == nil {
		t.Fatal("zero value must be reported invalid")
	}
	bad := identity.TaskID("tsk_not-a-uuid")
	if bad.Validate() == nil {
		t.Fatal("unchecked conversion must still fail Validate")
	}
	good := identity.NewTaskID()
	if good.IsZero() || good.Validate() != nil {
		t.Fatal("generated id must validate")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestNewPanicsWhenEntropyFails(t *testing.T) {
	uuid.SetRand(failingReader{})
	defer uuid.SetRand(nil)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New must panic (fail closed), not fall back to another format")
		}
	}()
	id := identity.NewRunID()
	t.Fatalf("expected panic, got %q", id)
}

func TestMigrationNamespaceConstant(t *testing.T) {
	const literal = "6570d821-e63e-592d-a51f-8cf4b43cdba5"
	derived := uuid.NewSHA1(uuid.NameSpaceDNS, []byte("rencrow.identity.migration.v1")).String()
	if derived != literal {
		t.Fatalf("documented derivation yields %s, not the fixed constant", derived)
	}
	if identity.MigrationNamespace != literal {
		t.Fatalf("MigrationNamespace=%s, want %s", identity.MigrationNamespace, literal)
	}
}

func TestMigrateIsDeterministicUUIDv5(t *testing.T) {
	// Expected values were produced independently with Python uuid.uuid5.
	got, err := identity.Migrate[identity.TaskKind]("legacy_tasks", "task_id", "legacy-123")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "tsk_77ad2309-4bdc-5181-afea-64eab42a39ea" {
		t.Fatalf("got %s", got)
	}
	run, err := identity.Migrate[identity.RunKind]("legacy_tasks", "task_id", "legacy-123")
	if err != nil {
		t.Fatal(err)
	}
	if run.String() != "run_ff800123-2888-56ec-9796-d081d095d810" {
		t.Fatalf("same legacy value must map differently per target type, got %s", run)
	}
	uni, err := identity.Migrate[identity.TaskKind]("tasks", "日本語", "値")
	if err != nil {
		t.Fatal(err)
	}
	if uni.String() != "tsk_aa4a9b1b-015b-5546-8bd6-90f670a26bda" {
		t.Fatalf("got %s", uni)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("migration id must satisfy the strict parser: %v", err)
	}
	again, _ := identity.Migrate[identity.TaskKind]("legacy_tasks", "task_id", "legacy-123")
	if again != got {
		t.Fatal("migration must be deterministic")
	}
}

func TestMigrateRejectsMissingParts(t *testing.T) {
	for _, in := range [][3]string{{"", "f", "v"}, {" ", "f", "v"}, {"t", "", "v"}, {"t", "\t", "v"}, {"t", "f", ""}} {
		if _, err := identity.Migrate[identity.TaskKind](in[0], in[1], in[2]); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestSchemaIDDefsAreAllCovered(t *testing.T) {
	var names []string
	for name := range schemaIDPatterns(t) {
		names = append(names, name)
	}
	sort.Strings(names)
	var ours []string
	for _, c := range cases() {
		ours = append(ours, c.typeName)
	}
	sort.Strings(ours)
	if strings.Join(names, ",") != strings.Join(ours, ",") {
		t.Fatalf("schema ID defs %v != identity types %v", names, ours)
	}
}
