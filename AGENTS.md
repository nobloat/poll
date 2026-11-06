# Agents

Follow the nobloat skills ([nobloat/skill](https://github.com/nobloat/skill), checked out next to this repo as `../skill`):

- `nobloat`: stack, code, HTTP, config, tests, UI and the plan-first workflow. Always.
- `nobloat-storage`: config, data layout, seeding, backup. For anything that writes files.
- `nobloat-outbox`: the outbox, SMTP, MIME, retries. For anything that sends mail.
- `nobloat-ui`: the shared look (`../ui/skill/SKILL.md`). For any page, template or style.

Agents without skill support: read `../skill/<name>/SKILL.md`.

## Workflow

Every change follows `nobloat` → "Change an existing project" (`../skill/nobloat/SKILL.md`):

1. The prompt (appended to `PROMPT.md` with the date).
2. Adapt or create `PLAN.md`, then `README.md`, then `wireframes.html`, each as if the change were already built.
3. Adapt or create the sample configs and seeds in `example/` to the new spec.
4. Clarify open topics: one numbered list, a recommended default per item.
5. Suggest the code layout and any interfaces (files, signatures, request and response types between services).
6. Wait for the owner's confirmation, then implement in one pass.
7. Automated tests, then end to end in the whole stack: [nobloat/system](https://github.com/nobloat/system) (`make local`, `make local-check`; from a sibling checkout `make local SRC=..`).
8. Adapt CI (`.github/workflows/ci.yml`) until it is green, push to `main`. Releases: `make release APP=<this repo>` in nobloat/system.

This repo is a submodule of nobloat/system, which checks out every component side by side; changes spanning several components are driven from there.
