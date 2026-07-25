# SPJ/OI contract v2 implementation plan

1. Add failing manifest/archive tests for v1 compatibility, OI score
   invariants, special-judge metadata, source references, digest and limits.
2. Add failing execution-config tests for ACM/OI/SPJ immutable snapshots and
   mismatch rejection.
3. Add failing batch-pipeline tests for full/partial OI scores, all-case
   execution, SPJ ABI, malformed checker results, and hidden-data redaction.
4. Extend callback and durable external result contracts with nullable score
   fields and validation.
5. Extend the external OpenAPI, capabilities and persistence contract for
   manifest v2 while retaining v1 clients.
6. Run race tests, static analysis, OpenAPI contract tests, integration tests,
   vulnerability checks and image build.
7. Verify Git author/committer identity, commit, push, open a draft PR and
   attach it to the v1.0.0 milestone.
