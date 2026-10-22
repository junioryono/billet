# Removing the guest releases published before #584

What was removed from this repository's releases on 2026-10-22, why, and what replaced it.

## Why

billet's guest image is published as a dated `guest-*` release that anyone can download, so billet redistributes everything in it. Until #602, every one of those images carried five components whose terms forbid billet redistributing them:

- the Android SDK (clause 3.4 of its license agreement);
- the CodeQL bundle (the GitHub CodeQL Terms);
- Google Chrome (the Google Terms of Service);
- Microsoft Edge and msedgedriver (their license terms);
- Firefox, the mozillateam PPA's build with a changed default preference, which Mozilla's permission for unaltered builds does not cover (ADR-005).

None of them carried the notices #621 adds or the corresponding source ADR-019 now publishes with every release. Keeping them downloadable kept redistributing all of that.

## What replaced them

`guest-20261022-191414`, published at 20:19 UTC on 2026-10-22 by guest-image run 39813415826 from v0.23.0, is the first image under the new rules:

- **Left out:** the five components are not in it. A deployment that accepts one in `images.accept` installs it on its own nodes (#596, #600, #602).
- **Notices:** it carries `/opt/billet/IMAGE_NOTICES` and the licenses it names (#621).
- **Source manifest:** it carries `/opt/billet/IMAGE_SOURCES.tsv`, published beside it as `image-sources.tsv` (#622).
- **Corresponding source:** the release holds it as `image-sources.tar` in two parts, with `image-sources.parts` and `image-sources-index.tsv` (#629, ADR-019).

The guest channel (`current.json` on the `guest-channel` branch) named it at 20:19:42 UTC, immutable and valid until 2026-11-01. Its 16 assets were read back before anything was removed: eight rootfs parts, the kernel, the signed manifest and its bundle, the source manifest, the source index, the parts list and the two source parts.

## What was removed

These 19 releases were deleted with `gh release delete` at about 20:20 UTC, minutes after the replacement was published. The first was deleted alone and read back before the rest: the release answered "release not found" and its tag remained.

| Release | Published |
|---|---|
| guest-20260903-084742 | 2026-09-03 08:55 UTC |
| guest-20260904-033328 | 2026-09-04 03:40 UTC |
| guest-20260905-012151 | 2026-09-05 01:27 UTC |
| guest-20260905-035306 | 2026-09-05 03:58 UTC |
| guest-20260907-053138 | 2026-09-07 05:35 UTC |
| guest-20260914-052534 | 2026-09-14 05:30 UTC |
| guest-20260921-054024 | 2026-09-21 05:44 UTC |
| guest-20260922-004207 | 2026-09-22 00:49 UTC |
| guest-20260923-073014 | 2026-09-23 07:35 UTC |
| guest-20260925-023242 | 2026-09-25 02:47 UTC |
| guest-20260926-053723 | 2026-09-26 05:44 UTC |
| guest-20260926-114239 | 2026-09-26 11:55 UTC |
| guest-20260928-063820 | 2026-09-28 06:53 UTC |
| guest-20261001-030940 | 2026-10-01 03:34 UTC |
| guest-20261004-040113 | 2026-10-04 04:28 UTC |
| guest-20261004-222429 | 2026-10-04 23:01 UTC |
| guest-20261005-070030 | 2026-10-05 07:26 UTC |
| guest-20261012-060307 | 2026-10-12 06:39 UTC |
| guest-20261012-080434 | 2026-10-12 08:34 UTC |

**The tags remain.** Each names the source commit its image was built from, which is provenance and not redistribution. GitHub does not let a tag an immutable release used be reused, so leaving them also keeps every name from being taken by a different release.

## What this does to a deployment

- **A generation already imported is untouched.** A deployment keeps the generations it has imported in its own store, until its own `billet images reap` removes them, and a tier pinned to one, or at `@verified` on one, goes on booting it.
- **By default nothing fetches an old tag.** `billet images pull` and the unattended refresh read the guest channel, which names the new release. A pull pointed at a removed release's assets now fails, because GitHub answers its download URLs with 404: by hand with `--source`, or on every refresh for a deployment whose `images.source` names one. Such a deployment goes back to the channel by removing `images.source`.
- **The known-answer run** downloaded a part of `guest-20261005-070030` as its network load. It was moved to the pinned Linux tarball first (#634).

## Decided by

The maintainer asked, on 2026-10-21, for #584 to be finished comprehensively, deciding open questions from research and from what GitHub and Blacksmith do. Neither GitHub nor Blacksmith publishes its runner image: GitHub publishes only the template, and its maintainers cite the licenses. Removing what billet may not redistribute, once a replacement that may be redistributed existed, follows from that.
