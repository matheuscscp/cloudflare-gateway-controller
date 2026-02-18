# What a Good Controller Looks Like

## Status Patching and Optimistic Locking

If a controller is the only one that patches the status of a given object, there is no
need for optimistic locking. The same applies to finalizers. For example, the Gateway
controller patching Gateway status and finalizers — when reconciling a Gateway, it will
never touch the status or finalizers of another Gateway object.

If a controller has to patch other objects (status or otherwise) that may be patched in
parallel by the controller itself or other controllers/entities when reconciling multiple
objects concurrently, we use optimistic locking. For example, the Gateway controller
patching GatewayClass finalizers and HTTPRoute statuses.

When optimistic locking is used, `RetryOnConflict` is always used together.

When `RetryOnConflict` is used, `NotFound` errors when `.Get()`ing the object being
patched must be ignored if the goal of the patch is to remove something (e.g. a finalizer
or a status entry), or to add an entry that becomes irrelevant when the object does not
exist (e.g. `RefNotPermitted` in the status of a deleted HTTPRoute).

## Conditions and kstatus

Every object should have a `Ready` condition following the kstatus standard:

- `Ready=True` indicates the object is fully ready.
- `Ready=False` indicates a terminal failure. A terminal state occurs only when the object
  configuration is invalid, e.g. strings that do not match an expected format, field
  combinations that are not allowed, etc. Infrastructure failures (API errors, network
  issues, etc.) are always transient.
- If we return a transient error to controller-runtime, we set `Ready=Unknown` with
  `Reason=ProgressingWithRetry` and `Message=<error>`.
- If we are waiting on managed objects to become ready, we set `Ready=Unknown` with
  `Reason=Progressing` and `Message=<what object(s) are we waiting for>`.

## Standardized APIs

In the particular case of a standardized API like Gateway API, we try as much as possible
to implement the conditions as recommended and avoid creating non-standard ones. But
status visibility is of utmost importance, so we try our best to create conditions that
fit well. For example: the `DNSRecordsApplied` condition on HTTPRoutes.

We also try as much as possible to avoid state tracking in the status. There is no space
for state tracking in the Gateway API — all it offers in status are conditions. In extreme
cases we may create a special condition for state tracking, but so far we have avoided
this by relying on the external state to compute the diff between actual and desired state.
For example: in the Gateway controller, the stale DNS records are discovered by reading
the actual state of DNS records in Cloudflare, and then comparing it to the desired state
computed from the attached HTTPRoutes.

## Minimizing Write Operations

We avoid making write operations as much as possible, regardless of whether they would be
status changes, changes in finalizers, or changes on external infrastructure. This is
achieved by first reading the actual state in order to compare it to the desired state.

The only exception is patching conditions on errors, to update the timestamp of conditions
with `Reason=Progressing(WithRetry)`, so users can see that progress is really being made.

## Watches and Indexes

A controller watches all the objects it depends on, but efficiency is of utmost importance
on watches. In particular, when watching large objects like CRDs, Secrets, and ConfigMaps,
we use `WatchesMetadata` (and disable cache for those objects). We rely on good indexes
and predicates to efficiently map watch events to reconcile requests.

Indexes can and should also be used if they can make the reconciliation faster. We do not
need to use them only for mapping watch events to reconcile requests.

## Logging

We log all errors. If we are going to return an error to controller-runtime in
`Reconcile()`, we let controller-runtime log it.

It is fine to have lots of debug-level logs. If we need users to debug, better not regret
a debug log that we did not ship.

## Events and Info/Error Logs

We always send an event for errors, or if something changed, but never if nothing changed
and the state is reconciled. We always try to summarize all errors or changes in a single
event to avoid spam.

Info and error level logs match events with `Normal` and `Warning` severities respectively,
both in the amount (to avoid spam) and in the exact place where events are emitted. Events
and info/error logs should be emitted at the same time.

## Errors Returned to controller-runtime

When returning an error from `Reconcile()`, controller-runtime will retry the reconciliation
with exponential backoff and log the error. Before returning the error, it is fine (and
encouraged) to best-effort patch conditions on the object (e.g. `Ready=Unknown` with
`Reason=ProgressingWithRetry`) and emit a `Warning` event with the error message. These
operations give users immediate visibility into what went wrong, while the returned error
ensures controller-runtime will keep retrying. If the best-effort status patch itself fails,
we log that failure but still return the original error.
