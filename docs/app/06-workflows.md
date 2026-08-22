# Workflows

A **workflow** is a list of steps that run in order, each one fed by what came
before. It is how you chain agents together — classify, then decide, then
draft — without a person in the middle.

Two kinds of step:

- **Agent step** — runs one of your agents. Its prompt is a template, so it
  can quote values produced earlier.
- **Code step** — a small JavaScript function. This is the glue: reshaping,
  filtering, arithmetic — the things you want done the same way every time.

## What a step can see

Every step receives the same three things:

| In a prompt | In code | What it is |
|---|---|---|
| `{{ .Input.customer }}` | `input.input.customer` | what you passed in when you ran it |
| `{{ .Previous.band }}` | `input.previous.band` | the step immediately before |
| `{{ .Steps.classify.sentiment }}` | `input.steps.classify.sentiment` | any earlier step, by name |

The third one is why steps have names. Without it, a value needed at step five
would have to be copied through steps two, three and four — and each copy is a
chance to lose it.

If you reference something that isn't there, the step **fails**. That is
deliberate: a prompt with a hole in it still gets a confident answer from a
model, and you would never know.

## An example

Three steps — classify a review, grade it, reply to the customer:

```json
{
  "name": "review triage",
  "steps": [
    { "name": "classify", "type": "agent", "agent_id": "…",
      "prompt": "Classify this review: {{ .Input.review }}" },

    { "name": "grade", "type": "code",
      "code": "function run(input) {\n  var s = input.steps.classify;\n  return { band: s.score >= 0.8 ? 'strong' : 'weak' };\n}" },

    { "name": "reply", "type": "agent", "agent_id": "…",
      "prompt": "Write a one-sentence reply to {{ .Input.customer }}. Their review was {{ .Steps.classify.sentiment }} with a {{ .Previous.band }} signal." }
  ]
}
```

Run it with the input it expects:

```bash
curl -X POST https://your-enact-host/workflows/{id}/executions \
  -H "Authorization: Bearer enact_sk_…" \
  -H "Content-Type: application/json" \
  -d '{"input": {"customer": "Ada", "review": "This changed how I work."}}'
```

## Agents that answer in JSON

An agent step's reply is stored as JSON when it *is* JSON, and as text
otherwise. So an agent with an **output schema** composes neatly: the next
step addresses its fields (`{{ .Steps.classify.sentiment }}`) instead of
receiving a paragraph it would have to parse.

Giving the first agent in a chain an output schema is usually the single
biggest improvement you can make to a workflow's reliability.

## Code steps

A code step defines a function called `run`:

```javascript
function run(input) {
  return { band: input.steps.classify.score >= 0.8 ? 'strong' : 'weak' };
}
```

It returns anything that can be expressed as JSON. It runs in a plain
JavaScript environment with **no access to the network, the filesystem, or
anything else** — the object it is given is all it can reach. It is stopped if
it runs too long, so an accidental infinite loop fails that step rather than
hanging the run.

## Running one, and reading what happened

Running is asynchronous. Triggering returns immediately with an execution id;
the run continues in the background. Fetch the execution to see how far it
has got — it is updated after every step, so you can watch progress rather
than waiting for a verdict.

Each execution records, per step: what it was given, what it produced, an
agent step's **rendered** prompt, and how long it took. That record is the
point. A chain of model calls that only tells you its final answer is
impossible to debug when the answer is wrong.

If a step fails, the run stops. That step is marked `failed` with the reason,
and the steps after it are marked `skipped` — so the record shows the whole
shape of the run and exactly where it stopped.

The step definitions are copied onto each execution when it starts. Editing a
workflow therefore never changes the history of a run that already happened.

## Permissions

Workflows follow the same rules as everything else, with one distinction worth
knowing: **being able to see a workflow is not being able to run it.** Running
one spends model calls, so it needs `use`, not just `view`.

Running a workflow does **not** grant you its agents. Each agent step runs as
*you*, and is refused if that agent isn't yours to use — so a workflow cannot
be a way around a permission you do not have. If you are given a workflow
whose agents you cannot use, its agent steps will fail.

## Limits worth knowing

- Triggering is manual for now — there is no schedule and no webhook yet.
- A workflow has a maximum number of steps, and each agent step can itself
  make several model calls, so a long workflow is an expensive one.
- Executions are kept indefinitely; there is no automatic cleanup yet.
