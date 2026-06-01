# AI Slop Taxonomy

Catalog of the patterns the **deslopper** skill targets: the structural and
tonal fingerprints of code written by earlier / weaker models. Each entry gives
the signal, a before/after, the fix, and the *false-positive guard* — when the
pattern is legitimate and must be left alone.

The governing rule for every category: **remediation is behavior-preserving.**
If removing slop would change an output, an interface, or an error path, it is
no longer a deslop — it is a behavior change and must be escalated separately.

---

## 1. Over-commenting

**Signal:** Comments that restate the code instead of explaining intent.

```go
// before
i++ // increment i by one
// Loop over all the users in the list
for _, u := range users { ... }

// after
i++
for _, u := range users { ... }
```

**Fix:** Delete comments that narrate *what*. Keep comments that explain *why*,
encode an invariant, link a spec/issue, or warn of a non-obvious gotcha.

**Not slop when:** the comment documents a surprising decision, a workaround for
an upstream bug (with link), a unit/units assumption, or a public API doc
comment (`///`, `/** */`, Go doc comments on exported symbols).

---

## 2. Dead scaffolding

**Signal:** Unused imports, variables, parameters; unreachable branches;
generation-left `TODO`/`FIXME`/`XXX`; commented-out code blocks; helpers that
are defined but called exactly once (or never).

```python
# before
import os  # unused
def handle(req, ctx, unused_opts=None):
    # TODO: implement caching
    result = compute(req)
    return result

# after
def handle(req, ctx):
    return compute(req)
```

**Fix:** Delete it. Dead code is a maintenance tax and a misdirection for the
next reader. Inline single-use trivial helpers when it improves locality.

**Not slop when:** the `TODO` references a real tracked ticket, the parameter is
required by an interface/signature contract, or the "unused" export is part of a
published API surface.

---

## 3. Redundant defensive code

**Signal:** Guards that the type system or an enforced caller contract already
guarantees; double-validation; try/except that catches only to re-raise
unchanged; belt-and-suspenders nil checks on values that cannot be nil.

```typescript
// before
function area(r: Rect): number {
  if (!r) return 0;            // r is non-optional — cannot be null here
  if (r.w == null) r.w = 0;    // w: number — never null
  return r.w * r.h;
}

// after
function area(r: Rect): number {
  return r.w * r.h;
}
```

**Fix:** Remove guards proven redundant by the static type or a contract the
codebase actually enforces.

**Not slop when:** the value crosses a real trust boundary — external input,
deserialized JSON, an `any`/`interface{}`/`unknown`, a public API parameter, or
anything from the network/disk. Defensive code at boundaries earns its keep.

---

## 4. Over-abstraction

**Signal:** Single-use wrapper functions that add nothing; one-implementation
interfaces; premature `Factory`/`Manager`/`Helper` indirection; layers that only
forward calls.

```go
// before
type UserGetter interface{ Get(id string) (*User, error) }
type dbUserGetter struct{ db *DB }
func (g *dbUserGetter) Get(id string) (*User, error) { return g.db.User(id) }
// ...only ever constructed as dbUserGetter, never mocked, one impl

// after
func getUser(db *DB, id string) (*User, error) { return db.User(id) }
```

**Fix:** Collapse indirection that has exactly one implementation and no test
seam. Inline pass-through wrappers.

**Not slop when:** the interface has (or imminently needs) multiple impls, is a
genuine test seam used by existing tests, or marks a real module boundary.

---

## 5. Copy-paste duplication

**Signal:** Near-identical blocks that differ only in a literal or two; the same
magic value repeated across a file.

**Fix:** Extract one function parameterized over the difference; hoist repeated
literals into a named constant. Use call-graph / text search to confirm the
blocks are truly equivalent before merging.

**Not slop when:** the blocks are coincidentally similar but semantically
independent (likely to diverge), or de-duplication would couple two modules that
should stay independent. Prefer a little duplication over the wrong abstraction.

---

## 6. Verbose boilerplate

**Signal:** Multi-line forms of one-line idioms.

```python
# before
def is_admin(u):
    if u.role == "admin":
        return True
    else:
        return False

result = []
for x in items:
    result.append(transform(x))

# after
def is_admin(u):
    return u.role == "admin"

result = [transform(x) for x in items]
```

**Fix:** Use the language's idiom: direct boolean returns, comprehensions /
`map`/`filter` where idiomatic in the repo, drop intermediate single-use vars.

**Not slop when:** the verbose form is genuinely clearer for a complex body, or
the repo's style explicitly avoids comprehensions/clever one-liners. Match the
surrounding code, not an abstract ideal.

---

## 7. Tonal slop

**Signal:** Marketing language and emoji in code/comments
("🚀 blazingly fast", "robust and comprehensive solution"), narration of how
great the code is, gratuitous debug logging, naming drift (`getUserData`,
`fetchUserInfo`, `loadUserRecord` for the same concept in one file).

**Fix:** Strip adjectives and emoji from code comments; remove self-congratulatory
narration; align names to one convention; cut logging that was scaffolding.

**Not slop when:** emoji/tone are an intentional part of user-facing output
(CLI UX, docs), not the source comments.

---

## Confidence & ordering

When remediating, rank by **(confidence × low-risk)**:

| Confidence | Examples |
|------------|----------|
| High | restating comments, unused imports, `if x: return True else: return False`, commented-out code |
| Medium | redundant guards, single-use wrappers, verbose loops |
| Low (read carefully) | "duplication" that may diverge, abstractions that may gain impls, guards near boundaries |

Do the high-confidence pass first — it is almost pure win and builds a clean
diff. Treat low-confidence items as judgment calls; when unsure, leave it and
note it in the slop report rather than risk a behavior change.
