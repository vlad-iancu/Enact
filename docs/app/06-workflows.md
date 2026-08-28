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
| `{{ .Input.customer }}` | `ctx.input.customer` | what you passed in when you ran it |
| `{{ .Previous.band }}` | `ctx.previous.band` | the step immediately before |
| `{{ .Steps.classify.sentiment }}` | `ctx.steps.classify.sentiment` | any earlier step, by name |

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
      "code": "function run(ctx) {\n  var s = ctx.steps.classify;\n  return { band: s.score >= 0.8 ? 'strong' : 'weak' };\n}" },

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

## Declaring shapes

A workflow can declare an **input schema** — a JSON Schema for the payload it
expects. It belongs to the workflow, not to a step, because every step sees
the same `ctx.input`:

```json
{
  "name": "review triage",
  "input_schema": {
    "type": "object",
    "properties": {
      "customer": { "type": "string" },
      "review":   { "type": "string" }
    },
    "required": ["customer", "review"],
    "additionalProperties": false
  },
  "steps": [ … ]
}
```

A run triggered with a payload that doesn't match is **refused immediately**,
with the offending field named — rather than starting, spending three model
calls, and failing somewhere in the middle.

A **code step** can declare an `output_schema` for what it returns, and it is
enforced: a return value that doesn't match fails that step. An unenforced
schema slowly stops describing reality, and then misleads every step after it.

An **agent step** does not declare one. Its output shape is the agent's own
output schema, taken from the agent itself — one contract in one place, rather
than two copies to keep in agreement. Set it on the agent.

Both schemas are checked when you save the workflow, so a broken schema is an
authoring error rather than something a run trips over.

### Resolved shapes

`GET /workflows/{id}/shapes` returns what every step produces and receives,
already resolved:

- **`output_schema`** per step, with **`output_source`** saying where it came
  from — `step` (a code step's own), `agent` (from the agent record), `text`
  (an agent with no schema, so prose: a JSON string), or `unknown` (a code
  step that declares nothing).
- **`context_schema`** per step: a JSON Schema for the `ctx` object *that*
  step will receive, with only the steps before it addressable.

That last one is meant for an editor. Convert it to a TypeScript type and a
code step's editor can complete `ctx.input.…` and `ctx.steps.…` accurately —
including which step names exist at that position, which is a rule you would
otherwise have to reimplement and keep in step with the runner.

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
function run(ctx) {
  return { band: ctx.steps.classify.score >= 0.8 ? 'strong' : 'weak' };
}
```

It returns anything that can be expressed as JSON. Name the parameter
whatever you like — `ctx` reads better than `input`, since the trigger payload
is `ctx.input`.

It runs in a plain JavaScript environment: the language and its standard
library — `Object`, `Array`, `JSON`, `Math`, `Date`, `RegExp`, `Map`, classes,
arrow functions, destructuring — and **nothing else**. There is no `fetch`, no
`require`, no `process`, no filesystem, no `window`. The object it is given is
all it can reach.

There is also **no event loop**, and therefore no `setTimeout` and nothing to
await. Do not mark `run` as `async`: a promise that is already settled is
honoured, but one waiting on anything can never settle here, and the step will
say so rather than hang. Return the value directly.

The code is checked for syntax errors when you **save** the workflow, so a
stray bracket is caught while you are writing it rather than halfway through a
run. And a step is stopped if it runs too long, so an accidental infinite loop
fails that step instead of hanging the workflow.

## Google Workspace steps

A **Google step** reads a file out of, or writes one into, *your own* Drive.
There are three — `google-docs`, `google-sheets` and `google-slides`. It names a **provider** — the same one you connect an account under —
and the credential is fetched at the moment the step runs, as whoever
triggered the workflow.

That last part is the whole design. The workflow stores no token: it says
*which* account to act through, and whose account that is depends entirely on
who ran it. Two people running the same workflow each reach their own Drive.
Someone who has never connected that provider gets a step that fails saying
so, rather than one that quietly acts as somebody else.

**Export** fetches a document as a file, which a later agent step can attach:

```json
{ "name": "brief", "type": "google-docs", "provider": "google",
  "operation": "export",
  "document_id": "{{ .Input.doc_id }}",
  "format": "pdf" }
```

It produces `{"file": {…}, "document_id": "…", "format": "pdf"}` — so the next
step reads the real document rather than a description of it:

```json
{ "name": "summarise", "type": "agent", "agent_id": "…",
  "prompt": "Summarise the attached brief.",
  "attach": ["steps.brief.file"] }
```

Formats depend on the type:

| step | exports as |
|---|---|
| `google-docs` | `pdf`, `docx`, `txt`, `html`, `md` |
| `google-sheets` | `pdf`, `xlsx`, `csv` *(csv is the first sheet only)* |
| `google-slides` | `pdf`, `pptx`, `txt` |

`pptx` is the one format no model can read — a deck exported that way can be
downloaded but not attached, so export a deck as `pdf` if an agent is meant to
read it.

The exported file is deleted with the execution, so a re-run fetches a fresh
copy — which is also what you want if the document changed.

**Create** writes a new document, usually at the end of a workflow:

```json
{ "name": "publish", "type": "google-docs", "provider": "google",
  "operation": "create",
  "title": "Summary — {{ .Input.customer }}",
  "body": "{{ .Steps.summarise }}" }
```

It produces `{"document_id": "…", "title": "…", "url": "…"}`, so a later step —
or a person reading the run — can open it.

A **spreadsheet** takes `rows` instead of a body, as a template rendering to a
JSON array of arrays — usually straight from an earlier step:

```json
{ "name": "report", "type": "google-sheets", "provider": "google",
  "operation": "create",
  "title": "Weekly report",
  "rows": "{{ .Steps.summarise.rows }}" }
```

A **presentation** is created empty. Giving it content would mean choosing a
slide layout, which is a decision this step should not make for you.

**Append** is Sheets only, and is how a workflow logs what it did:

```json
{ "name": "log", "type": "google-sheets", "provider": "google",
  "operation": "append",
  "document_id": "{{ .Input.sheet_id }}",
  "rows": "[[\"{{ .Input.customer }}\", \"{{ .Steps.classify.sentiment }}\"]]" }
```

It adds rows after the last one already there, and reports where they landed:
`{"document_id": "…", "updated_range": "Sheet1!A7:B7", "appended_rows": 1}`.
Values are entered as a person would type them, so `=SUM(A1:A9)` becomes a
formula and `5` a number. Set `range` (for example `"Sheet1!A:C"`) to target a
particular sheet.

Both operations template their fields, and both are checked when you save:
a broken template, an unknown format, or a provider that does not exist is an
error while you are writing the workflow, not several minutes into a run.

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
