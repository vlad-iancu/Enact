# Crawls

A **crawl** fills a retrieval knowledge base from the web, by itself, and
keeps it up to date.

You give it a question in plain language, a page to start from, and an empty
retrieval knowledge base. It reads the starting page, works out which of its
links are most likely to lead somewhere relevant, follows the best one, and
repeats — keeping the pages that are about your topic and ignoring the rest.
Then it does the whole thing again on a schedule.

## Creating one

Four things:

- **A query.** Ordinary language: *"sea otter habitat, diet and
  conservation"*. This is not a search box — it is the description the crawl
  measures every page against, so a sentence works better than keywords.
- **A starting URL.** Usually the most relevant page you already know.
- **An empty retrieval knowledge base.** It must be of the *retrieval* kind
  and it must be empty; see [Two kinds of knowledge base](getting-started).
- **How often to repeat**, or *manual only* if you would rather start it
  yourself.

The crawl stays on the starting page's own site unless you widen it. That is
what keeps a crawl focused: relevance decides the *order* it explores in, but
something has to decide the *extent*, and "the site you pointed me at" is
what most people mean.

### Why the knowledge base must be empty

The crawl takes ownership of it. It remembers which document came from which
page, so that on the next run it can leave unchanged pages alone and replace
only what actually changed. That bookkeeping is only true if nothing else put
documents there — otherwise the crawl would eventually delete somebody else's
uploads believing they were pages that had vanished.

If you want to combine a crawl with hand-picked documents, use two knowledge
bases, or upload by hand only after the crawl exists and accept that the
crawl may remove them.

## When a site hides its text

The crawler works out where a page's content is from the shape of the
document. That is right for articles and wrong for applications: a JIRA
ticket, a wiki page, an admin console or a docs site built entirely from
`div`s has no obvious "main" region, and what comes back is either nothing or
the navigation menu.

**Extraction rules** settle it. Each rule is a URL pattern and a list of CSS
selectors, and on pages whose URL matches the pattern, the text is taken from
the elements the selectors name:

| | |
|---|---|
| **URL pattern** | `https://jira.example.com/browse/*` — `*` stands for any run of characters, including slashes |
| **Selectors** | `#jira-issue-header h1`, `.description` — any CSS selector |

Rules are tried in order and the first match wins, so put the specific pattern
above the general one. A crawl with no rules behaves exactly as it always has.

Three things worth knowing:

- **Rules change the text, not the links.** The crawl still finds links
  anywhere on the page, so a selector chosen to capture a ticket's description
  will not accidentally decide where the crawl may go next.
- **A rule that matches nothing falls back.** If a site is redesigned and your
  selectors stop matching, those pages are read the old way rather than coming
  back empty.
- **The report says which pages used a rule.** If a rule is not doing what you
  expected, that is the first place to look.

## Crawling an issue tracker

A crawl can explore a JIRA project instead of a website. The shape is the same
— a query, somewhere to start, an empty knowledge base — and only what a
"starting point" means changes: an issue key like `SCRUM-1`, or a browse URL
containing one.

From there it follows the relationships that mean *part of the same piece of
work*: an issue's parent, its subtasks, and the links somebody deliberately
drew between issues. It does **not** follow issues merely mentioned in text,
which would drag it across the whole backlog.

How far it follows is the thing to set. **`jira.max_depth` defaults to 2 and
cannot exceed 4**, and it is a tighter limit than it sounds: one hop from an
epic is all its children, and another is all their subtasks and everything
those link to. Issue relationships are reciprocal, so each hop multiplies
rather than adds. Two is usually a whole piece of work.

You will need the site URL, the email of the account the token belongs to, and
an API token. Either kind works — Atlassian issues **classic** tokens and
**scoped** ones, they look identical, and the crawl works out which it has on
its first call. A classic token authenticates with the email and token
together, so a classic token with the wrong email fails exactly as a wrong
token does; a scoped one needs `read:jira-work`. The token is
stored the same way as any other credential: encrypted, and never readable
again.

The credentials are checked once at the start of every run, and a run that
fails with *"rejected the API token"* means exactly that — the token has been
revoked, or the email does not match the account it belongs to. This check
exists because the alternative is worse: Jira answers "issue does not exist or
you do not have permission to see it" for **every** issue when it does not
recognise you, which reads like a missing issue and sends you looking for one.

## Sites that need a login

A crawl can present request headers to sites that require them — a JIRA API
token, an internal wiki's session header. Each entry is a URL pattern and a set
of headers, and the headers are sent only to URLs matching the pattern.

The pattern **must name a host**. `https://jira.example.com/*` is fine, and so
is `https://*.example.com/`; `https://*` is refused. A crawl follows links and
links leave sites, so a credential attached to "this crawl" rather than to a
host would eventually be handed to a stranger because somebody put a link in a
ticket.

Four things the platform guarantees about them:

- **They are encrypted at rest**, under a key separate from the one protecting
  your connected accounts.
- **They are never readable again.** Listing a crawl shows the header *names*
  and the URLs they go to, so you can see what a crawl is configured to send,
  and never the values.
- **They do not follow redirects.** If an authenticated page redirects
  somewhere else, the headers are dropped for that hop — even to another
  subdomain of the same site.
- **They cannot rewrite the crawler's identity.** `User-Agent`, `Host` and the
  transport's own headers are refused, because sites tolerate being crawled on
  the basis that the crawler says honestly what it is.

Editing a crawl without re-sending the headers leaves them in place, so you can
change a query without handling secrets again. Sending an empty list clears
them.

**Not a login form.** These are headers, not a sign-in flow: a site that needs
a username and password typed into a page is not reachable this way, and a
token that expires will need replacing.

## What the bounds do

Every crawl is bounded, and you can tighten any of them:

| | |
|---|---|
| **Max pages** | How many pages one run may fetch. |
| **Max depth** | How many links from the starting page it may travel. |
| **Max duration** | Wall clock for one run. |
| **Score threshold** | How relevant a page must be to be kept — and, more importantly, how promising a link must be for the crawl to bother following it. |

The threshold is the interesting one. It is not only a filter: when the most
promising unvisited link falls below it, the crawl **stops**, because nothing
left anywhere is worth fetching. Raise it for a tighter, smaller corpus;
lower it to range wider.

## Reading the report

Every run produces a report, and it is worth reading — especially the first
one, because it shows you what the crawl *thought you meant*.

**The query, understood.** Each significant word is tagged and resolved to a
specific meaning, with the definition it settled on. This is where you find
out that "conservation" was read in the physics sense rather than the
environmental one, or that "diet" was read as slimming rather than as what an
animal eats. Nothing else in the system will tell you that, and it explains
more surprising crawls than anything else.

The meanings are chosen together rather than one at a time, and the crawl reads
your **starting pages** before it reads your query — so "index" next to a page
full of shards, clusters and mappings is understood differently from "index"
next to a page about book publishing. The report lists that vocabulary under
the query, which is worth a glance: if the starting page was mostly navigation
and not much text, there was little for the crawl to go on, and pointing it at
a content-heavy page instead usually fixes more than any setting.

If a word still comes out wrong, the next thing to try is the query itself. A
sentence that says what field you are in — *"OpenSearch search engine software:
database index mapping and query syntax"* rather than *"opensearch indices and
syntax"* — gives it much more to work with than a list of keywords.

Occasionally the report says the query was understood in **reduced** form.
That means the richer of the two dictionaries was unavailable — usually its
daily allowance was spent — so the crawl fell back to the smaller one built
in. It still runs, but with a plainer vocabulary and no knowledge of named
products or technologies, so it may follow different links than usual. The
next run normally has the full dictionary back.

**The query, expanded.** The synonyms, broader terms, narrower terms and
related forms the crawl also looked for. This is why a page about "marine
mammal range" can match a query about otters.

**The graph.** Every page the crawl reached, what it scored, and how they link
together — including the pages it *decided not to fetch*, each with a reason:
below the threshold, off-site, too deep, or out of budget. If a crawl missed
something you expected, it is usually visible here as a link that scored too
low or fell outside a bound.

Each score comes in two parts, and the split matters:

- **Semantic** — how close the page's *meaning* is to your query.
- **Lexical** — how much of your query's *vocabulary* the page actually uses.

A page scoring well on lexical alone repeats your words without being about
them. A page scoring well on semantic alone is about your topic in different
words — often exactly what you wanted and what a keyword search would miss.

## Repeat runs

A repeat run is cheap and non-destructive:

- a page whose content has not changed is left completely alone;
- a page that has changed replaces its previous version;
- a page that has disappeared is removed, but only after it has been missed
  several runs in a row — a page missed once because the crawl ran out of
  budget is not gone.

## When a run says "partial"

Not a failure. It means the crawl made progress and stopped with work still
to do — it hit the page limit, the time limit, or the daily allowance of the
language service it consults while reading pages. The list of pages it had
queued is saved, and the next run picks up exactly where it left off instead
of starting over.

Running out of that allowance while interpreting the *query* is different: it
does not stop the run, it reduces it. See "reduced" in [Reading the
report](#reading-the-report).

The first run of a brand-new crawl is the most likely to be partial, because
the query has never been analysed before. Once it has, that work is cached
and later runs are much cheaper.

## Being a good citizen

Crawls run from the platform's own address, so the sites you point them at see
the platform rather than you. That makes politeness the platform's
responsibility, and it is not optional:

- `robots.txt` is fetched and obeyed, including any crawl delay it asks for;
- requests to one site are spaced out, and only a few run at once;
- the crawler identifies itself honestly in its User-Agent;
- crawls cannot reach private or internal addresses, whatever a URL claims.

Point crawls at sites whose terms allow it, and prefer a modest page budget.

## Permissions

A crawl is a resource like any other. Being able to *see* one is not the same
as being able to *run* it — a run makes requests to other people's sites and
costs money to index what it finds, so running requires the `use` permission
specifically. See [Organizations and permissions](organizations-and-permissions).

A run always acts as the crawl's **owner**, not as whoever pressed the button,
because it is the owner's knowledge base being written to.
