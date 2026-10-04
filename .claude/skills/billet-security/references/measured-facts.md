# Measured facts

Part of the `billet-security` skill: what was measured, dated where it was recorded.

- `%+v` on `awscreds.IMDS` printed the secret access key and session token through an unexported field.
- 29 redaction mutations run; three pre-existing gaps found (`Chain`'s three renderers, and both provider clients' tests holding members that could not leak).
- A CodeBuild service role holding only `ssm:GetParameters` resolves the parameter; `ssm:GetParameter` was dropped from the grant.
- `169.254.170.2` serves the build's role credentials inside a VPC on-demand build.
- `os.Root` opened a relative `link.lock -> real.lock` as `os.SameFile` with its target.
- 31 on-demand CodeBuild builds: 31 distinct host boot ids (2026-09-02); reserved fleets keep a marker written by one build readable by the next.
