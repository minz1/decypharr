// Package notifications delivers events to the configured notifiers (a
// Discord webhook, an HTTP callback).
//
// Each notifier has its own actor on the service's task group: it sends one
// event at a time, in the order Notify saw them, so a slow or failing
// notifier never holds up another. Delivery is at-least-once: an event is
// sent up to 4 times (the first send and 3 retries, 1s, 5s and 30s after the
// failure before), each send bounded by 10s. A network error, a timeout, 429
// and 5xx are retried; a 429 or 5xx Retry-After longer than the backoff is
// honoured up to 30s. Any other response, or a request that cannot be built,
// ends the event at once. A receiver may see an event twice when a response
// is lost after it was handled; events carry no idempotency key.
//
// At most 100 events wait behind the one being sent; past that the oldest is
// dropped, with one WARN per overflow and an INFO when the queue drains.
// Stopping the group cancels the sends in flight and drops what is queued.
package notifications
