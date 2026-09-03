# Acceptance records

What ran for real, when, and what it found. These are the evidence behind [What is proven](../status.md); every claim in them names a date, an account or a host.

```{toctree}
:maxdepth: 1

aws-acceptance
site-acceptance
restore-rehearsal
```

| Record | Covers |
|---|---|
| [AWS acceptance](aws-acceptance.md) | cold end-to-end jobs on EC2 from a private repository, the `billet acceptance` command that turned the hand procedure into one, spot interruption, same-label failover, and what the CodeBuild API actually does |
| [Site acceptance](site-acceptance.md) | the site boundary proved on real hosts: three nodes, two sites, one deployment, and the two defects only a real AWS could show |
| [Restore rehearsal](restore-rehearsal.md) | why the two CI rehearsal legs exist, what each proves that the other cannot, and what neither proves |
