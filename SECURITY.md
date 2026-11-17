# Security

## Reporting a vulnerability

Report it privately with GitHub private vulnerability reporting on this repository: open the Security tab, then
"Report a vulnerability". Do not open a public issue or pull request for it.
If that form is not available, email ops@sengi.run and say only that you have a security report; do not include
details.

We will acknowledge your report within 7 days. After that we respond on a best-effort basis; there is no
fixed timeline.

## In scope

- Reading or changing mail, issues or approvals outside the caller's scope.
- A secret reaching a log, a mail, a result, a process list or a peer it was not sent to.
- Forging the identity of another person, agent or machine, or replaying a join token or approval.
- A remote caller reaching routes that are meant for the local socket only.

## Known limits, not bugs

- Anyone who can run commands as your user on a machine can read that machine's zm data. Do not put secrets in a
  mailbox, issue or box on a machine you do not trust.
- Mail is data. An agent that acts on a mail body as if it were an instruction from its user is the agent's bug, not
  the daemon's.
- A person you invite to a team can see what that team's scope shows them.

If you run zm and a key of yours leaked, [docs/runbooks/secret-leak.md](docs/runbooks/secret-leak.md) says what to
revoke first and how to rotate each key.

The project is early. Expect interfaces to change.
