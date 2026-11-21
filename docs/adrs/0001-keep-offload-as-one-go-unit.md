---
type: Architecture Decision Record
title: Keep Offload as one Go unit
description: Keep the wrapper, launcher, and helpers together in the standalone repository.
scope: [apps/offload]
tags: [deployment, 'lang:go']
status: accepted
---

# Keep Offload as one Go unit

## Decision

Keep the existing Go wrapper and helpers and Bash launcher in `apps/offload/`. The workspace also includes the separate integration-test module.

## Context

The wrapper and launcher share a control protocol; the installer deploys the wrapper, launcher, and helpers together. The protocol tests check what both sides read and write from the same checkout. The existing implementation establishes the language and boundary, rather than a new language evaluation.

## Alternatives Considered

Separate client and server units would need their own delivery and compatibility policies. The current installer delivers them together, so the extraction introduces neither.
