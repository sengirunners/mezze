# Engineering principles

Agents write and maintain all of this code, many at once, with no memory between them (an owner
rule). Make the next agent's change small, local and safe. These are concepts, not
metrics; a design regression is a review finding, not style.

1. **One concept, one home.** Search before writing a helper, rule, query or type; import or extend
   it. Copies drift apart.
2. **The producer enforces the invariant.** Validity, visibility and correction rules live where the
   data comes from, so every reader inherits them.
3. **Split by what changes together.** An entry point assembles; a feature's state, rules and
   interface live in a unit named for it. A child unreadable without its parent is no boundary.
4. **Extend through a seam.** When a new kind needs edits in N places, first build the one table or
   interface they consult; a one-caller flag is not a seam.
5. **Store facts once; derive the rest.** A record of what was decided or published is a fact.
6. **Bad states unrepresentable, missing data loud.** Validate at the boundary, then trust the type;
   no placeholder for a missing entity, no empty string for an id.
7. **Finish the change.** A move deletes the old path; a removal takes every layer that only served
   it.
8. **Refactor first, separately.** New code never goes inline in a file too big to read whole; move
   what it needs out first, in its own PR. Fixes and reverts are exempt (file the move).
9. **Comments say what is true now and why**, never history; **tests go through a public
   interface**, never source text.

Act, don't wait: fix what is wrong on your path; file what would double the PR; never stop at a
report an agent could act on. Never meet a check by moving lines without their state, trimming lines
or raising a number.
