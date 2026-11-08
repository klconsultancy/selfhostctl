# Domain docs

How the engineering skills use this repo's domain documentation.

## Read first

- `GLOSSARY.md`: the project's terms.
- `docs/design.md`: the one design document. Read the sections that touch the area you work in.

If either is missing, carry on without saying so.

## Rules

- **Glossary terms:** name domain concepts as `GLOSSARY.md` defines them, in issue titles, proposals and test names. Don't drift to the synonyms it lists under _Avoid_. A concept that isn't in it is either invented language (reconsider) or a gap (note it for `/domain-modeling`).
- **No ADRs:** decisions go into `docs/design.md`, as tables and short bullets, not prose. Change a section when the design moves; don't append history.
- **Conflicts:** when your output contradicts `docs/design.md`, say so explicitly instead of overriding it silently.
