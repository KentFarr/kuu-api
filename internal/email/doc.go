// Package email handles outbound transactional mail.
//
// Mailer wraps net/smtp with header sanitisation and a strict recipient
// validator (no display-name form, no CRLF). Sending is asynchronous: the
// provisioner enqueues a row in pending_emails, and Queue.Run reads claims
// with FOR UPDATE SKIP LOCKED, sends, and updates the row. On failure the
// next attempt is scheduled with quadratic backoff capped at maxBackoff.
//
// RenderWelcome produces the body shown to customers; the same sanitiser
// used for headers is applied to user-controlled fields embedded in the
// body, so a hostile username can't inject MIME headers.
package email
