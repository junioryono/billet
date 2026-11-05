# Weights between targets on replay

What `targets[].weight` does to contended room (#530), replayed through billet's real listener, allocator, placer and node over the simulated backend at compressed time, on the shared Linux test host on 2026-11-04. A weight is meant to divide room only while targets contend for it, so each run is measured over its contested window: from when both targets held room until the first of them had no job left to start.

## How it was replayed

Two targets, one tier each, 2 vCPU and 8 GiB, on the same hosts of 16 vCPU and 64 GiB. Each tier's stream alone offers 1.5 times everything the fleet holds, with runtimes lognormal around 4 minutes (σ 0.4), so both are saturated throughout. The same seeded trace is replayed three ways: weighted 2:1, with no weight written, and with the second target's jobs removed. The test is `TestContendedRoomIsDividedByWeight`, at about eighty jobs a stream in the ordinary suite and at the sizes below for this record. Every job started and finished in every replay, and the ledger proved no overcommit.

All of it ran under GitHub.com's direct assignment. Under GitHub Enterprise Server's offers no tier is recorded as waiting (#686), so the order has nobody to rank and a weight does nothing there. The replay's other limits are [the ones the policy comparison states](replay-policies.md#how-it-was-replayed-and-what-that-leaves-out). In particular the queue's own clock is wall time, so a replay never reaches the 15 minutes after which a waiter stands above every rank, or the backoff after a failed launch: those are held by unit tests, not by this record.

## Two targets at 2:1

The share of the contested vCPU the first target held, over eight even slices of the window. Memory charges the same shape, so its shares are the same.

| Fleet | Jobs a stream | Weights | Contested | First target's vCPU-hours | Second's | Ratio | Share by slice |
|---|---|---|---|---|---|---|---|
| 2 hosts, 16 slots | 1,000 | 2:1 | 7h42m | 159.2 | 87.0 | 1.83 | 64%, then 65% in each of the other seven |
| 2 hosts, 16 slots | 1,000 | none | 10h03m | 159.6 | 162.0 | 0.99 | 48% to 51% |
| 8 hosts, 64 slots | 3,000 | 2:1 | 5h42m | 480.9 | 247.6 | 1.94 | 65% to 66% |
| 8 hosts, 64 slots | 3,000 | none | 7h30m | 481.2 | 478.4 | 1.01 | 50% |

It converges at once and stays there. It falls short of 2:1 by the size of a lease: 16 slots can hold 11 and 5 or 10 and 6, never 10⅔ and 5⅓, and on four times the slots the ratio moves from 1.83 to 1.94. The weighted runs' windows are shorter only because the first target runs out of jobs sooner.

## One target idle

With the second target's jobs removed and its weight still written, the first held a mean of 32.0 of 32 vCPU on two hosts and 128.0 of 128 on eight, from the start that filled the room to its last. An idle target's weight claims nothing.

## What a replay of equal shapes could not show

The first version of the order ranked every waiter on every ask, and equal shapes measured as well under it as above: 64% to 36%. Two reviews then traced that it broke what makes the order end, and two more replays, with mixed shapes and floors, prove it:

- **A wide shape keeps its turn.** `TestAWideShapeGetsItsTurnUnderWeights`: a tier half a host wide at twice the weight of a stream of small jobs. Ranked on every ask, a small job ending put its target behind again, it bought the room back, and the wide tier's second job started only when its first ended, 40m30s later. A waiter first when it was refused now keeps the line until it buys, and the two run together.
- **A floor above a target's weighted share is kept.** `TestAFloorAboveAWeightedShareIsKept`: a tier of weight 1 reserved half the fleet beside a target of weight 2. Unprotected it held 36% of the contested vCPU, with its reserved room idle; a tier below its floor now goes first, and the test holds it at 48% or more with the fleet in use.

## What it does not say

- It divides room between two targets of one shape. Dominant share across shapes is held by `TestATargetsRankIsItsDominantShareOverItsWeight` against a fixed ledger, not measured here.
- These runs predate the two later fixes to the order, a bounded backoff after a failed launch and a floor counted as the floor is on a draining host. Neither arises here: no launch fails and no host drains.
- A weight of 2 is a claim on contended room, not a guarantee of two thirds of a deployment: a target with little work uses what it needs, and the patience above means a tier cannot be starved however large its rival's weight.

## Reproducing it

`BILLET_REPLAY_JOBS=1000 BILLET_REPLAY_HOSTS=2 go test -run '^TestContendedRoomIsDividedByWeight$' ./internal/replay -args -replay.out DIR` writes the tables to `DIR/target-weights-2hosts-weighted.md`, `-even.md` and `-idle.md`, the three replays running side by side. With `TMPDIR` on tmpfs the two-host run took 5 minutes and the eight-host run 34, one after another.
