package service

import "github.com/Nyukimin/RenCrow_Harness/pkg/protocol"

// Capabilities (initialize and service/capabilities) lists what this build can do.
// A capability is ready only if it is implemented here; everything else is
// unavailable with the reason. Nothing is reported as contract_tested or
// runtime_verified: no acceptance test has run against this build, so the basis is
// what the build itself declares.
func (s *Service) Capabilities() protocol.CapabilitiesResult {
	ready := func(name string, reason *string) protocol.Capability {
		return protocol.Capability{Name: name, Status: "ready", Basis: "declared", Reason: reason}
	}
	missing := func(name, reason string) protocol.Capability {
		return protocol.Capability{Name: name, Status: "unavailable", Basis: "declared", Reason: protocol.Str(reason)}
	}
	startReason := "admission only: the turn, task and run are recorded durably but the run is not executed in this build"
	if s.model != nil {
		startReason = "admits the turn, task and run durably and starts the run on the model port this process was given"
	}
	resumeReason := "admits a new run of an ended task; the run is not executed in this build, as turn/start's is not"
	if s.model != nil {
		resumeReason = "admits a new run of an ended task, with a new trace and explicit limits, and starts it on the model port this process was given; a tool call of unknown outcome is never run again, and a task with a generation of unknown outcome gets a run that is blocked at once"
	}
	caps := []protocol.Capability{
		ready("initialize", nil),
		ready("service/capabilities", nil),
		ready("session/open", nil),
		ready("session/list", nil),
		ready("session/get", nil),
		s.forkCapability(missing),
		ready("turn/start", protocol.Str(startReason)),
		ready("input/append", protocol.Str("stores one more input of the running run with its classification (next_step, next_turn, interrupt_current); the driver applies it, or the next run of the thread does")),
		ready("turn/interrupt", protocol.Str("records the stop signal of a run: a record, not proof that anything stopped; the run's driver stops its work and records how the run ended")),
		ready("run/get", nil),
		ready("run/resume", protocol.Str(resumeReason)),
		s.compactMethodCapability(missing),
		ready("receipt/get", nil),
		ready("events/read", nil),
		ready("evidence/read", nil),
		ready("service/shutdown", nil),
		s.compactionCapability(missing),
	}
	caps = append(caps, s.modelCapabilities(missing)...)
	caps = append(caps, s.toolCapabilities(missing)...)
	return protocol.CapabilitiesResult{ProtocolVersion: protocol.ProtocolVersion, BuildRevision: s.build, Capabilities: caps}
}

// compactionCapability reports whether a Run of this process compacts a prompt that does not
// fit, and before it is full. It does where the process has a model port and the deployment
// enabled it; what the binding offers (the strict contract, a verified count, the stage
// profiles) is asked when a Run needs a compaction, and a Run that finds it missing ends with
// the code that says so. The manual operation, context/compact, is a method of its own and is
// reported as one.
func (s *Service) compactionCapability(missing func(name, reason string) protocol.Capability) protocol.Capability {
	switch {
	case s.model == nil:
		return missing("context.compaction", "this process has no model port, so no run executes here and none compacts its prompt")
	case !s.dep.Config.Compaction.Enabled:
		return missing("context.compaction", "the deployment disables compaction (compaction.enabled=false): a prompt that does not fit ends the run blocked for capacity")
	}
	return protocol.Capability{Name: "context.compaction", Status: "ready", Basis: "declared", Reason: protocol.Str(
		"a run whose prompt does not fit compacts it once for the step, and a run whose verified prompt has reached compaction.trigger_ratio of the usable budget does so before it is full (a compaction that comes to no checkpoint then lets the run go on with the prompt that fits): Selection and Summary (each asked at most once, never retried) and then a checkpoint, or the reduction that needs no model; what the binding gives (strict contract, verified count) is asked when a run needs it, and a run that finds it missing ends with the code that says so")}
}

// compactMethodCapability reports context/compact: the manual compaction of an idle Thread,
// and its dry run. It needs what any compaction needs, a model port to count and ask, and the
// deployment's switch.
func (s *Service) compactMethodCapability(missing func(name, reason string) protocol.Capability) protocol.Capability {
	switch {
	case s.model == nil:
		return missing("context/compact", "this process has no model port, so it cannot count or compact a context")
	case !s.dep.Config.Compaction.Enabled:
		return missing("context/compact", "the deployment disables compaction (compaction.enabled=false)")
	}
	return protocol.Capability{Name: "context/compact", Status: "ready", Basis: "declared", Reason: protocol.Str(
		"compacts an idle thread in a system task and run of its own, with the result in the receipt (receipt/get); dry_run only counts the context and stores the receipt; a busy thread is BUSY, never queued behind")}
}

// forkCapability reports session/fork: a new Thread from a checkpoint of another, in the same
// session. It needs a model port, because the copied projection's count is held to the new
// Thread's binding.
func (s *Service) forkCapability(missing func(name, reason string) protocol.Capability) protocol.Capability {
	if s.model == nil {
		return missing("session/fork", "this process has no model port, so it cannot count the copied context for the new thread")
	}
	return protocol.Capability{Name: "session/fork", Status: "ready", Basis: "declared", Reason: protocol.Str(
		"makes a new thread in the same session from a checkpoint of the source thread, which imports the sources the checkpoint names; copies no active run, tool state, queued input or file state, and restores no files")}
}

// modelCapabilities reports whether a Run of this process is executed and generates: it is
// when the process was given a model port (the RenCrow_LLM Gateway client of `serve`, or a
// test's double), and not otherwise (a read-only process, which admits nothing to run).
// What it declares is the composition, not the Gateway's state: whether the Gateway can be
// reached and offers the Run's binding is asked when a Run starts (the description of its
// binding), and a Run that finds it cannot ends blocked with MODEL_UNAVAILABLE. Nothing here
// says the model side was tried.
func (s *Service) modelCapabilities(missing func(name, reason string) protocol.Capability) []protocol.Capability {
	if s.model == nil {
		return []protocol.Capability{
			missing("turn.execution", "this process has no model port, so a run admitted here is not executed and stays in phase Admitting"),
			missing("model.generation", "this process has no model port"),
		}
	}
	return []protocol.Capability{
		{Name: "turn.execution", Status: "ready", Basis: "declared", Reason: protocol.Str("a run admitted here is executed to its end on the model port this process was given: act with at most one retry, Tool calls, and every failure ending the run as it classifies")},
		{Name: "model.generation", Status: "ready", Basis: "declared", Reason: protocol.Str("one strict generation per attempt, each counted first; the Gateway and the binding are asked when a run starts, not when the process starts, so this does not say the Gateway is reachable")},
	}
}

// toolCapabilities reports the Tool runtime: whether a Run of this process can use Tools,
// and in which modes. The six Tools exist in this build, but they only run inside a Run,
// and a process without a model port runs none; isolated has no adapter and is never
// stood in for by another mode.
func (s *Service) toolCapabilities(missing func(name, reason string) protocol.Capability) []protocol.Capability {
	const modes = "structured_only: file Tools and evidence.read; trusted_host: those and process.exec, with no OS isolation; isolated: unavailable"
	if s.model == nil {
		return []protocol.Capability{
			missing("tool.runtime", "the Tool runtime exists but runs only inside a Run, and this process has no model port: "+modes),
			missing("tool.runtime.structured_only", "no model port in this process"),
			missing("tool.runtime.trusted_host", "no model port in this process"),
			missing("tool.runtime.isolated", "isolated needs an accepted isolation adapter; this build has none and does not fall back to another mode"),
		}
	}
	return []protocol.Capability{
		{Name: "tool.runtime", Status: "ready", Basis: "declared", Reason: protocol.Str(modes)},
		{Name: "tool.runtime.structured_only", Status: "ready", Basis: "declared", Reason: protocol.Str("file.read, file.search, file.create, file.edit and evidence.read; process.exec is never offered")},
		{Name: "tool.runtime.trusted_host", Status: "ready", Basis: "declared", Reason: protocol.Str("adds process.exec where the policy has process profiles; the child runs as the same OS user and nothing isolates it")},
		missing("tool.runtime.isolated", "isolated needs an accepted isolation adapter; this build has none and does not fall back to another mode"),
	}
}
