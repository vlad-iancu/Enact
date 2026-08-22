# ADR-0020: Workflows run on a queue, against a copy of their own definition

**Date**: 2026-08-21
**Status**: accepted
**Deciders**: Vlad Iancu, Claude

## Context

Agents were invokable one at a time. Chaining them — classify, decide, draft —
had to happen outside the platform, which meant the intermediate steps were
neither stored nor authorized. A workflow makes that chain a resource: an
ordered list of steps, where a step is either an agent invocation with a
templated prompt or a JavaScript function over the previous results.

Three forces shaped the design. A chain of agent steps can run for minutes,
each agent step running its own tool loop. Code steps are arbitrary
user-authored JavaScript. And a workflow is a way to spend model calls against
agents, so it sits directly on top of the authorization model.

## Decision

Workflows are authored and triggered by **enact-workflows**, and executed by a
separate **enact-workflow-runner** consuming a Redis stream. Triggering writes
an execution record, publishes its id, and returns 202. Each step's input,
output and timing is written to that record as the run proceeds. The step
definitions are **copied onto the execution** when it is queued. Code steps
run in goja with no host access and a wall-clock interrupt.

## Alternatives Considered

### Synchronous execution inside the API
- **Pros**: no queue, no second binary, the result is the response.
- **Cons**: a multi-minute HTTP request; a client disconnect abandons a run
  mid-way with model calls already paid for; user JavaScript burning CPU on
  the host that serves interactive requests.
- **Why not**: the durations are not incidental to this feature, they are its
  normal case.

### Reading the workflow at execution time instead of copying it
- **Pros**: one source of truth; an edit applies to in-flight runs.
- **Cons**: a run could execute a definition that changed underneath it, and
  a stored execution would be read against steps it never ran. Deleting the
  workflow would make its history unreadable.
- **Why not**: an execution record whose steps are not the steps that ran is
  worse than no record.

### A restricted expression language instead of JavaScript
- **Pros**: no arbitrary code execution, trivially bounded.
- **Cons**: every non-trivial transformation becomes a feature request, and
  the escape hatch people actually want is a real language.
- **Why not**: the isolation goja gives — no I/O of any kind reachable from a
  script — covers the risk that matters, and the CPU risk is answered by
  running it in the worker rather than the API.

### Passing only the previous step's output to each step
- **Pros**: simplest possible mental model.
- **Cons**: any value needed later must be threaded through every intervening
  step, and each hop is a chance to drop it.
- **Why not**: the full context costs nothing to build and removes a whole
  class of authoring mistake.

## Consequences

### Positive
- A run survives a disconnected client, and the record shows progress per step
  rather than only a final verdict.
- Authorization is **inherited, not re-implemented**: the runner acts as the
  triggering user, and `enact-model-inference` already refuses an agent that
  user may not use. A workflow therefore cannot launder a permission.
- Editing a workflow never rewrites the history of a run that already
  happened.
- An agent with an `output_schema` composes: its reply is stored as JSON, so
  the next step addresses fields instead of parsing prose.

### Negative
- Two more binaries, a second Redis stream, and a polling client.
- A trigger returns before anything has run, so "it was accepted" and "it
  worked" are different questions.
- The step definitions are duplicated on every execution record.

### Risks
- **Cost is bounded only by the step cap.** Steps × tool-loop turns can be a
  hundred model calls, and nothing meters an execution's spend. `MaxSteps`
  exists; a token budget does not.
- **goja is a language sandbox, not a process one.** A script reaches nothing,
  but it consumes a core until the interrupt fires. Mitigated by running in
  the worker, not the API.
- **Deleting a workflow while a run is in flight** can leave that one
  execution record behind: the cascade proceeds past documents the runner is
  concurrently writing rather than aborting (see `DeleteByQuery`,
  `conflicts=proceed`). The record is orphaned, not duplicated, and no listing
  reaches it.
- Executions accumulate with no retention policy.
