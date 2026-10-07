package protocol_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func TestStartInputSchemaRejections(t *testing.T) {
	base := example(t, "core_start.json")
	cases := map[string]func(m map[string]any){
		"unknown field":                       func(m map[string]any) { m["extra"] = true },
		"unknown nested field":                func(m map[string]any) { m["limits"].(map[string]any)["extra"] = 1 },
		"missing limits":                      func(m map[string]any) { delete(m, "limits") },
		"missing upstream (null is required)": func(m map[string]any) { delete(m, "upstream") },
		"wrong type":                          func(m map[string]any) { m["expected_context_revision"] = "0" },
		"fractional integer":                  func(m map[string]any) { m["expected_control_revision"] = json.Number("0.5") },
		"negative revision":                   func(m map[string]any) { m["expected_context_revision"] = -1 },
		"thread id of another kind":           func(m map[string]any) { m["thread_id"] = "tsk_00000000-0000-7000-8000-000000000001" },
		"uppercase id":                        func(m map[string]any) { m["thread_id"] = "thr_00000000-0000-7000-8000-00000000000A" },
		"uuid v4 id":                          func(m map[string]any) { m["thread_id"] = "thr_00000000-0000-4000-8000-000000000001" },
		"short idempotency key":               func(m map[string]any) { m["idempotency_key"] = "short" },
		"zero deadline":                       func(m map[string]any) { m["limits"].(map[string]any)["deadline_seconds"] = 0 },
		"zero generation attempts":            func(m map[string]any) { m["limits"].(map[string]any)["max_generation_attempts"] = 0 },
		"origin human without proof object": func(m map[string]any) {
			m["input"].(map[string]any)["origin"] = "human"
		},
		"proof mac not hex": func(m map[string]any) {
			m["input"].(map[string]any)["origin_proof"] = map[string]any{"mac": "xyz"}
		},
		"too many blocks": func(m map[string]any) {
			blocks := make([]any, 257)
			for i := range blocks {
				blocks[i] = m["context_blocks"].([]any)[0]
			}
			m["context_blocks"] = blocks
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.Decode[protocol.StartInput](mutate(t, base, f))
			wantCode(t, err, protocol.CodeInvalidParams)
		})
	}
}

func TestDecodeRejectsMalformedJSONBeforeSchema(t *testing.T) {
	base := string(example(t, "start_result.json"))
	cases := map[string][]byte{
		"duplicate key":      []byte(strings.Replace(base, `"accepted": true`, `"accepted": true, "accepted": true`, 1)),
		"invalid utf-8":      []byte(strings.Replace(base, `"accepted"`, "\"acc\xffepted\"", 1)),
		"unpaired surrogate": []byte(strings.Replace(base, `"accepted"`, `"acc\ud800epted"`, 1)),
		"bom":                append([]byte("\xef\xbb\xbf"), base...),
		"trailing value":     []byte(base + "{}"),
		"truncated":          []byte(base[:len(base)/2]),
		"empty":              nil,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.Decode[protocol.StartResult](data)
			wantCode(t, err, protocol.CodeInvalidParams)
		})
	}
}

func TestDecodeErrorsDoNotEchoValues(t *testing.T) {
	const secret = "sk-live-do-not-echo-0123456789"
	base := example(t, "core_start.json")
	for name, f := range map[string]func(m map[string]any){
		"unknown field value": func(m map[string]any) { m["extra"] = secret },
		"wrong type value":    func(m map[string]any) { m["thread_id"] = secret },
		"bad key value":       func(m map[string]any) { m["idempotency_key"] = secret + "!" },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.Decode[protocol.StartInput](mutate(t, base, f))
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error repeats the offending value: %v", err)
			}
			var pe *protocol.Error
			if !errors.As(err, &pe) || strings.Contains(pe.Message, secret) {
				t.Fatalf("client-facing message leaks the value: %v", err)
			}
		})
	}
}

func TestStartInputUserMessageBlockIsInvalidRequest(t *testing.T) {
	base := example(t, "core_start.json")
	withBlock := func(m map[string]any) {
		m["context_blocks"] = append(m["context_blocks"].([]any), map[string]any{
			"kind": "user_message", "text": "dup", "revision": "ctx-v1:" + strings.Repeat("0", 64), "source": nil,
		})
	}
	_, err := protocol.Decode[protocol.StartInput](mutate(t, base, withBlock))
	wantCode(t, err, protocol.CodeInvalidRequest)
}

func TestStartInputBlockRevisionMustMatchContent(t *testing.T) {
	base := example(t, "core_start.json")
	cases := map[string]func(m map[string]any){
		"text changed": func(m map[string]any) { m["context_blocks"].([]any)[0].(map[string]any)["text"] = "changed" },
		"revision changed": func(m map[string]any) {
			m["context_blocks"].([]any)[1].(map[string]any)["revision"] = "ctx-v1:" + strings.Repeat("a", 64)
		},
		"kind changed": func(m map[string]any) {
			m["context_blocks"].([]any)[2].(map[string]any)["kind"] = "variable_runtime_context"
		},
		"source added": func(m map[string]any) {
			m["context_blocks"].([]any)[3].(map[string]any)["source"] = map[string]any{
				"owner": "RenCrow_CORE", "source_id": "x", "raw_hash": strings.Repeat("a", 64), "projection_version": "text/v1",
				"range": map[string]any{"start": 0, "end": 1}, "origin": "human", "sequence": 1,
			}
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.Decode[protocol.StartInput](mutate(t, base, f))
			wantCode(t, err, protocol.CodeInvalidRequest)
		})
	}
}

func TestStartInputSourceRangeAndProofTimes(t *testing.T) {
	relay := example(t, "core_relay_start.json")
	for name, f := range map[string]func(m map[string]any){
		"impossible date": func(m map[string]any) {
			m["input"].(map[string]any)["origin_proof"].(map[string]any)["issued_at"] = "2026-13-40T00:00:00Z"
		},
		"expires before issue": func(m map[string]any) {
			p := m["input"].(map[string]any)["origin_proof"].(map[string]any)
			p["issued_at"], p["expires_at"] = "2026-10-07T00:05:00Z", "2026-10-07T00:00:00Z"
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.Decode[protocol.StartInput](mutate(t, relay, f))
			if err == nil {
				t.Fatal("accepted")
			}
			if c := protocol.CodeOf(err); c != protocol.CodeInvalidRequest && c != protocol.CodeInvalidParams {
				t.Fatalf("code %s", c)
			}
		})
	}
}

func TestVerificationPassedNeedsEvidenceAndCriteria(t *testing.T) {
	base := example(t, "run_result.json")
	passed := func(evidence []any, criteria any) func(m map[string]any) {
		return func(m map[string]any) {
			m["verification"] = map[string]any{"status": "passed", "evidence_ids": evidence, "criteria_revision": criteria}
		}
	}
	good := []any{"evd_00000000-0000-7000-8000-000000000001"}
	if _, err := protocol.Decode[protocol.RunResult](mutate(t, base, passed(good, "criteria-1"))); err != nil {
		t.Fatalf("a well-formed passed verification was refused: %v", err)
	}
	for name, f := range map[string]func(m map[string]any){
		"no evidence":    passed([]any{}, "criteria-1"),
		"no criteria":    passed(good, nil),
		"empty criteria": passed(good, ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.Decode[protocol.RunResult](mutate(t, base, f))
			if err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// The Go-side check stands on its own, for values that never went through Decode.
	if err := (protocol.Verification{Status: "passed"}).Validate(); err == nil {
		t.Fatal("Verification.Validate accepted passed without evidence")
	}
}

func runInfo(t testing.TB, f func(m map[string]any)) []byte {
	t.Helper()
	base := map[string]any{
		"run_id": "run_00000000-0000-7000-8000-000000000001", "task_id": "tsk_00000000-0000-7000-8000-000000000001",
		"thread_id": "thr_00000000-0000-7000-8000-000000000001", "phase": "Generating", "terminal": false, "result": nil,
		"context_revision": 1, "control_revision": 0, "last_event_seq": 3,
		"effective_limits": map[string]any{"max_model_steps": 10, "max_tool_calls_per_step": 8, "deadline_seconds": 1800, "max_capture_bytes": 67108864, "max_generation_attempts": 32},
		"deadline_at":      "2026-10-07T00:30:00Z", "recovery_policy_revision": strings.Repeat("a", 64),
		"generation_attempts_used": 2, "generation_attempts_unknown": 1,
	}
	f(base)
	b, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunInfoTerminalAndResultCorrespond(t *testing.T) {
	result := func() map[string]any {
		var r map[string]any
		if err := json.Unmarshal(example(t, "run_result.json"), &r); err != nil {
			t.Fatal(err)
		}
		r["run_id"] = "run_00000000-0000-7000-8000-000000000001"
		r["task_id"] = "tsk_00000000-0000-7000-8000-000000000001"
		return r
	}
	if _, err := protocol.Decode[protocol.RunInfo](runInfo(t, func(map[string]any) {})); err != nil {
		t.Fatalf("running RunInfo refused: %v", err)
	}
	terminal := func(m map[string]any) { m["terminal"], m["phase"], m["result"] = true, "Terminal", result() }
	if _, err := protocol.Decode[protocol.RunInfo](runInfo(t, terminal)); err != nil {
		t.Fatalf("terminal RunInfo refused: %v", err)
	}
	for name, f := range map[string]func(m map[string]any){
		"terminal without result":    func(m map[string]any) { m["terminal"], m["phase"] = true, "Terminal" },
		"terminal with wrong phase":  func(m map[string]any) { terminal(m); m["phase"] = "Generating" },
		"running with a result":      func(m map[string]any) { m["result"] = result() },
		"phase Terminal but running": func(m map[string]any) { m["phase"] = "Terminal" },
		"result of another run": func(m map[string]any) {
			terminal(m)
			m["result"].(map[string]any)["run_id"] = "run_00000000-0000-7000-8000-000000000999"
		},
		"result of another task": func(m map[string]any) {
			terminal(m)
			m["result"].(map[string]any)["task_id"] = "tsk_00000000-0000-7000-8000-000000000999"
		},
		"unknown above used":           func(m map[string]any) { m["generation_attempts_unknown"] = 3 },
		"unknown phase":                func(m map[string]any) { m["phase"] = "Thinking" },
		"deadline not a timestamp":     func(m map[string]any) { m["deadline_at"] = "2026-13-45T00:00:00Z" },
		"RetryWaiting is a real phase": nil,
	} {
		if f == nil {
			if _, err := protocol.Decode[protocol.RunInfo](runInfo(t, func(m map[string]any) { m["phase"] = "RetryWaiting" })); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			continue
		}
		t.Run(name, func(t *testing.T) {
			if _, err := protocol.Decode[protocol.RunInfo](runInfo(t, f)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestCompactResultConditions(t *testing.T) {
	list := rawList(t, "compact_results.json")
	normal, dry := list[0], list[8]
	capacity := list[2]
	for name, raw := range map[string][]byte{
		"normal with an unverified budget": mutate(t, normal, func(m map[string]any) { m["before"].(map[string]any)["state"] = "estimated" }),
		"normal with semantic beyond durable": mutate(t, normal, func(m map[string]any) {
			m["semantic_boundary"], m["durable_boundary"] = 30, 20
		}),
		"normal without checkpoint": mutate(t, normal, func(m map[string]any) { m["checkpoint_id"] = nil }),
		"dry_run with an outcome":   mutate(t, dry, func(m map[string]any) { m["outcome"] = "NormalCompacted" }),
		"dry_run with a checkpoint": mutate(t, dry, func(m map[string]any) { m["checkpoint_id"] = "ckp_00000000-0000-7000-8000-000000000002" }),
		"capacity numbers alone": mutate(t, capacity, func(m map[string]any) {
			m["required_minimum_tokens"] = 100
		}),
		"capacity numbers without a shortfall": mutate(t, capacity, func(m map[string]any) {
			m["required_minimum_tokens"], m["available_tokens"] = 100, 100
		}),
		"inverted interval": mutate(t, normal, func(m map[string]any) {
			m["before"].(map[string]any)["prompt_lower"], m["before"].(map[string]any)["prompt_upper"] = 3000, 2000
		}),
		"exact budget with unequal counts": mutate(t, normal, func(m map[string]any) { m["before"].(map[string]any)["prompt_upper"] = 2500 }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := protocol.Decode[protocol.CompactResult](raw); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	ok := mutate(t, capacity, func(m map[string]any) { m["required_minimum_tokens"], m["available_tokens"] = 9000, 8000 })
	if _, err := protocol.Decode[protocol.CompactResult](ok); err != nil {
		t.Fatalf("a real shortfall was refused: %v", err)
	}
}

func TestIntakeReceiptOriginFacts(t *testing.T) {
	base := example(t, "intake_receipt.json")
	digest := strings.Repeat("c", 64)
	for name, f := range map[string]func(m map[string]any){
		"relay without a digest":       func(m map[string]any) { m["proof_basis"] = "verified_relay" },
		"digest without a relay":       func(m map[string]any) { m["proof_digest"] = digest },
		"declared_local as automation": func(m map[string]any) { m["effective_origin"] = "automation" },
		"automation basis as human":    func(m map[string]any) { m["proof_basis"], m["declared_origin"] = "automation", "automation" },
		"unknown basis as human":       func(m map[string]any) { m["proof_basis"] = "unknown" },
		"unknown origin on a relay": func(m map[string]any) {
			m["proof_basis"], m["proof_digest"], m["effective_origin"] = "verified_relay", digest, "unknown"
		},
		"bad entrypoint":         func(m map[string]any) { m["entrypoint"] = "stdio_human" },
		"bad principal":          func(m map[string]any) { m["principal"] = "Ren" },
		"accepted_at not a time": func(m map[string]any) { m["accepted_at"] = "2026-02-30T00:00:00Z" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := protocol.Decode[protocol.IntakeReceipt](mutate(t, base, f)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// A human declaration capped to automation by the profile is a legitimate receipt.
	capped := mutate(t, base, func(m map[string]any) { m["effective_origin"], m["proof_basis"] = "automation", "automation" })
	if _, err := protocol.Decode[protocol.IntakeReceipt](capped); err != nil {
		t.Fatalf("declared human, effective automation was refused: %v", err)
	}
	relay := mutate(t, base, func(m map[string]any) {
		m["proof_basis"], m["proof_digest"], m["effective_origin"] = "verified_relay", digest, "human"
	})
	if _, err := protocol.Decode[protocol.IntakeReceipt](relay); err != nil {
		t.Fatalf("verified relay refused: %v", err)
	}
}

func TestEvidenceReadRanges(t *testing.T) {
	req := func(start, end uint64) []byte {
		return []byte(`{"evidence_id":"evd_00000000-0000-7000-8000-000000000001","projection_version":"text/v1","range":{"start":` + strconv.FormatUint(start, 10) + `,"end":` + strconv.FormatUint(end, 10) + `}}`)
	}
	if _, err := protocol.Decode[protocol.EvidenceReadInput](req(0, 65536)); err != nil {
		t.Fatalf("64 KiB is allowed: %v", err)
	}
	if _, err := protocol.Decode[protocol.EvidenceReadInput](req(10, 10)); err != nil {
		t.Fatalf("an empty range is allowed: %v", err)
	}
	for name, data := range map[string][]byte{"too long": req(0, 65537), "inverted": req(5, 4)} {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.Decode[protocol.EvidenceReadInput](data)
			wantCode(t, err, protocol.CodeInvalidRange)
		})
	}
}

func TestEvidenceReadResultConsistency(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("hello"))
	result := func(f func(m map[string]any)) []byte {
		m := map[string]any{
			"evidence_id": "evd_00000000-0000-7000-8000-000000000001", "projection_version": "raw/v1", "data_base64": data,
			"total_bytes": 5, "returned_range": map[string]any{"start": 0, "end": 5}, "partial": false,
			"raw_hash": strings.Repeat("a", 64), "projection_hash": strings.Repeat("b", 64), "capture_complete": true,
		}
		f(m)
		b, _ := json.Marshal(m)
		return b
	}
	if _, err := protocol.Decode[protocol.EvidenceReadResult](result(func(map[string]any) {})); err != nil {
		t.Fatalf("whole evidence refused: %v", err)
	}
	for name, f := range map[string]func(m map[string]any){
		"bytes shorter than the range": func(m map[string]any) {
			m["returned_range"] = map[string]any{"start": 0, "end": 6}
			m["total_bytes"] = 6
			m["partial"] = false
		},
		"range past the end": func(m map[string]any) {
			m["returned_range"] = map[string]any{"start": 0, "end": 5}
			m["total_bytes"] = 4
		},
		"part returned but not partial": func(m map[string]any) {
			m["total_bytes"] = 10
		},
		"whole but flagged partial":      func(m map[string]any) { m["partial"] = true },
		"incomplete capture not partial": func(m map[string]any) { m["capture_complete"] = false },
		"not base64":                     func(m map[string]any) { m["data_base64"] = "!!!" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := protocol.Decode[protocol.EvidenceReadResult](result(f)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	partial := result(func(m map[string]any) { m["total_bytes"] = 10; m["partial"] = true })
	if _, err := protocol.Decode[protocol.EvidenceReadResult](partial); err != nil {
		t.Fatalf("a partial read refused: %v", err)
	}
}

func TestEncodeRefusesMalformedValues(t *testing.T) {
	var ok protocol.StartResult
	if err := json.Unmarshal(example(t, "start_result.json"), &ok); err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.Encode(ok); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.RunID = "run_not-a-uuid"
	if _, err := protocol.Encode(bad); err == nil {
		t.Fatal("Encode accepted a malformed ID")
	}
	bad = ok
	bad.EffectiveLimits.DeadlineSeconds = 0
	if _, err := protocol.Encode(bad); err == nil {
		t.Fatal("Encode accepted a zero deadline")
	}
	bad = ok
	bad.Intake.ProofBasis = protocol.ProofBasisVerifiedRelay // digest still nil
	if _, err := protocol.Encode(bad); err == nil {
		t.Fatal("Encode accepted a relay without a digest")
	}
	var empty protocol.StartInput
	empty.ContextBlocks = nil
	if _, err := protocol.Encode(empty); err == nil {
		t.Fatal("Encode accepted a zero StartInput")
	}
}

func TestReceiptPayloadRoundTrip(t *testing.T) {
	var sr protocol.StartResult
	if err := json.Unmarshal(example(t, "start_result.json"), &sr); err != nil {
		t.Fatal(err)
	}
	p, err := protocol.NewReceiptPayload(sr)
	if err != nil || p.Type != protocol.ReceiptStartResult {
		t.Fatalf("%v %v", p, err)
	}
	raw, err := protocol.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := protocol.Decode[protocol.ReceiptPayload](raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := back.StartResult()
	if err != nil || got != sr {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := protocol.NewReceiptPayload("not a result"); err == nil {
		t.Fatal("accepted a non-result")
	}
	if _, err := (protocol.ReceiptPayload{Type: protocol.ReceiptRunResult, Value: json.RawMessage(`{}`)}).StartResult(); err == nil {
		t.Fatal("read a RunResult as a StartResult")
	}
}

func TestOriginProofDigestMatchesVector(t *testing.T) {
	var v struct {
		Proof          protocol.OriginProof `json:"proof"`
		MACInputSHA256 string               `json:"mac_input_sha256"`
	}
	exampleJSON(t, "origin_proof_vector.json", &v)
	got, err := protocol.OriginProofDigest(v.Proof)
	if err != nil || got != v.MACInputSHA256 {
		t.Fatalf("got %s, %v; want %s", got, err, v.MACInputSHA256)
	}
	other := v.Proof
	other.MAC = strings.Repeat("0", 64)
	if again, _ := protocol.OriginProofDigest(other); again != got {
		t.Fatal("the digest must not depend on the MAC")
	}
	other = v.Proof
	other.Nonce = "fixture-nonce-00000002"
	if again, _ := protocol.OriginProofDigest(other); again == got {
		t.Fatal("the digest must depend on the nonce")
	}
}
