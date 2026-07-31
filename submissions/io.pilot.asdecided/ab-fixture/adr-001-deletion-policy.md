# ADR-001: Account Deletion Policy

## ID

ADR-001

## Status

Accepted

## Context

Closed user accounts must remain auditable while personal data is removed according to policy.

## Decision

Do not hard-delete closed user accounts. Mark the account closed and run the approved personal-data erasure workflow.

## Consequences

Account records remain available for audit, and callers must use the closure workflow instead of direct deletion.
