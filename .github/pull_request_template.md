## Summary

<!-- What changed, and why? Link the issue or ADR when one exists. -->

## Tests run

<!-- Exact commands and results. Do not write "tests pass" without the output. -->

```text
$ make test
...
```

## Risk and rollback

<!-- What can break? How is it detected? How would a maintainer revert it? -->

## Checklist

- [ ] Tests were added or updated for the behavior change, or this is a
      documentation-only change.
- [ ] `make check` (or the relevant focused commands) passes locally.
- [ ] Public endpoints, chart values, stored schema, and support lists are
      unchanged, or the change and its compatibility impact are documented.
- [ ] Dependency, image, and GitHub Actions version bumps are in a separate PR
      with human review, per `CONTRIBUTING.md`.
- [ ] Security-sensitive changes (SQL emitter/escaping, Helm securityContext or
      capabilities, NetworkPolicy, release/signing) are flagged for human review.
- [ ] No credentials, generated binaries, or rendered charts are committed.
