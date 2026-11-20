# nobloat/poll plan

What poll does is described in [README.md](README.md). This file records decisions.

## Abuse limits and retention (owner decision 2026-11-20, review fixes POLL-1 to POLL-13)

Decided by the owner:

- Max 500 answers per poll (409 beyond).
- Mail: 5 per hour per recipient address, 200 per hour in total; excess is not queued, only logged. This includes
  the outcome mails when a poll closes, so closing a poll with more than 200 participants who left an email sends
  only the first 200 within the hour.
- Proof of work: 16 bits on every POST, 20 bits on requests that mail an address typed into the form (`POST /new`
  with an email, `POST /p/{id}` with an email). The challenge carries its difficulty (`issued.bits.rand.mac`, HMAC
  signed); `GET /pow?mail=1` hands out a 20-bit one. Challenge TTL 10 minutes.
- Polls are deleted 180 days after their last activity; a sweep runs at start and daily.
- `\r` and `\n` are rejected in titles and names. ICS escaping also turns a bare CR into `\n`.
- Outbox: give up after 3 days or on an SMTP 5xx; the job moves to `data/mail/failed/` and is logged. Due times are
  kept in memory after one directory read at start.
- Admin basic auth: 10 failures in 10 minutes per client IP → 429 until the window passes; failures are logged.
- `ADMIN_EMAIL` (optional) is the owner email of polls the admin creates without typing one.

Decided by the fixer (safest option that keeps the stack working):

- Last activity is the poll file's modification time (every create, answer, edit, close writes the file); no new field.
- Public poll creation (without admin auth) is capped at 100 per hour in total (429), so anonymous creation cannot fill
  the disk faster than the sweep empties it.
- Owner notifications carry the participant's choices and the manage link, no longer all results (POLL-1).
- "Changed" notifications are sent only when the choices change, at most once per answer per 10 minutes (POLL-6).
- Answering with an address that already answered redirects to the same "check your mail" page as a new answer with
  an email and mails that address its existing link (POLL-10). A new answer with an email no longer shows the
  personal link on the page; it is only in the mail. Answers without email still show it.
- Only an SMTP 5xx to MAIL, RCPT or DATA is permanent; a 5xx while connecting, on STARTTLS or AUTH is our config and
  keeps being retried (until the 3 days run out).
- The client IP for the auth throttle is the last `X-Forwarded-For` entry (caddy sets it), else the peer address.
  No minimum admin password length, because the local stack uses `admin`.
- Changes to one poll are serialised by one of 64 mutexes picked by a hash of its ID; reads take no lock (writes are
  atomic renames). The admin list skips and logs unreadable files.
- Spent PoW challenges are kept in two maps rotated every TTL; more than 100 000 in one TTL fail closed.
- Autosave retries once with a fresh proof when a save answers 400 (expired proof after a long-open tab).
